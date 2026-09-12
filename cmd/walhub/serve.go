// serve.go — `walhub serve` (the default): the §10.4 startup order of
// 06_server_http.md — bootstrap decision tree, store by backend, registry,
// engine, api env, chi server, loops gated by server.roles, prewarm,
// watchdog, plain-HTTP/h2c listener, and the two-phase drain on SIGTERM/SIGINT.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/events"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/maintain"
	"git.packden.us/crueber/walhub/internal/notify"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// runServe boots the server (§10.4). It returns the process exit code.
func runServe(ctx context.Context, c *cli, args []string) int {
	dataDir := dataDirFor(c)
	if err := config.EnsureDataDir(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: data dir: %v\n", err)
		return exitErr
	}

	cfg, fileState, err := resolveConfig(c)
	if err != nil {
		if fileState == stateAbsent {
			// Missing explicitly-named file (§6.1: exit 2).
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitArg
		}
		// INVALID config → SETUP-ONLY MODE (§3.4): only /setup, /healthz,
		// /readyz, /services/public answer; the setup UI displays the errors.
		fmt.Fprintf(os.Stderr, "walhub: config invalid — entering setup-only mode: %v\n", err)
		boot := server.BootState{Mode: "setup_only", Errors: []string{err.Error()}, ConfigPaths: configCandidates(c)}
		cfg = config.FirstRunDefaults(dataDir)
		applyPortOverride(cfg) // setup-only still honors the PORT lockstep
		return serveHTTP(ctx, cfg, boot, dataDir, nil)
	}
	// The --data-dir flag is authoritative over the XDG/env default (§3.1.1):
	// re-point the flag-derived PATHS so first-run store/cache and setup saves
	// live in the directory the operator named. The loaded config keeps every
	// file/env value (backend, auth, …) — re-deriving FirstRunDefaults here
	// would silently discard the WALHUB__* overlay.
	envDataDir := config.ResolveDataDir(os.Getenv)
	cfg.DataDir = dataDir
	if cfg.Store.Root == filepath.Join(envDataDir, "store") {
		cfg.Store.Root = filepath.Join(dataDir, "store")
	}
	if cfg.Cache.Dir == filepath.Join(envDataDir, "cache") {
		cfg.Cache.Dir = filepath.Join(dataDir, "cache")
	}
	bootMode := "normal"
	if fileState == stateAbsent {
		bootMode = "defaults"
		logSetupBanner(cfg)
	}
	for _, w := range mustWarnings(cfg) {
		slog.Warn(w) // the §5-rule-1 auth-none-on-non-loopback warning
	}

	st, err := openStore(cfg, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: store: %v\n", err)
		return exitErr
	}
	return serveHTTP(ctx, cfg, server.BootState{Mode: bootMode, ConfigPaths: configCandidates(c)}, dataDir, st)
}

// serveHTTP assembles AppState and runs the listener until drain completes.
// st is nil in setup-only mode (no store, engine, api, or loops).
func serveHTTP(ctx context.Context, cfg *config.Config, boot server.BootState, dataDir string, st store.ObjectStore) int {
	log := newLogger(cfg)
	setupOnly := boot.InSetupOnly()

	var reg *wal.Registry
	var engine *server.WalEngine
	var apiEnv *api.Env
	var collab *collabWiring
	if !setupOnly {
		wal.SetWarnLogger(func(format string, args ...any) { log.Warn(fmt.Sprintf(format, args...)) })
		reg = wal.NewRegistry(ctx, st, cfg)
		engine = server.NewWalEngine(reg, cfg)
		apiEnv = api.NewEnv(st, &repoRegistry{reg: reg, st: st}, cfg, engine, version(), hostname())
		apiEnv.Tasks = &opsTasks{reg: reg, eng: engine}
		apiEnv.Instance = reg.InstanceID()
		// Explicit create-repo placeholder (#210 §4): one hint set shared
		// by the api create paths (Add) and the server push pipeline
		// (Consume) — same process, so adoption needs no push-path read.
		apiEnv.PlaceholderHints = &api.PlaceholderHints{}
		// Features 01–08 assemble in exactly one place (collab.go,
		// 09 §4 touch point 3); integration tests reuse buildCollab
		// so the measured composition is the shipped composition.
		collab = buildCollab(st, cfg, reg, apiEnv)
	}

	// ---- events bridge before server.New (§10.4 order: AppState then loops) --
	roles := roleSet(cfg.Server.Roles)
	eventsRole := len(roles) == 0 || roles["events"]
	var pullsSvc *pulls.Service
	var notifySvc *notify.Service
	var ident *identity.Service
	if collab != nil {
		pullsSvc, notifySvc, ident = collab.pullsSvc, collab.notifySvc, collab.ident
	}
	var wake func(repo string)
	if !setupOnly && eventsRole && (cfg.Events.WebhookURL != "" || pullsSvc != nil) {
		var sinks []events.Sink
		if cfg.Events.WebhookURL != "" {
			sinks = append(sinks, &events.WebhookSink{URL: cfg.Events.WebhookURL, Secret: cfg.Events.WebhookSecret})
		}
		if pullsSvc != nil {
			// The pulls invalidation sink rides the bridge (Seam 4);
			// its Deliver never fails, so it never holds back the
			// shared cursor.
			sinks = append(sinks, &pullsSinkAdapter{svc: pullsSvc})
		}
		bridge := events.New(events.Deps{
			Source:        events.NewRegistrySource(reg),
			Store:         reg.Store(),
			Sinks:         sinks,
			SweepInterval: time.Duration(cfg.Events.SweepInterval),
			Logger:        log,
		})
		go bridge.Run(ctx)
		wake = func(repo string) { bridge.Wake(repo) }
	}

	srv := server.New(server.Options{
		Config:    cfg,
		Store:     st,
		Engine:    engine,
		API:       newAPI(apiEnv),
		DataDir:   dataDir,
		CacheRoot: cfg.Cache.Dir,
		Version:   version(),
		Instance:  instanceID(cfg),
		Kind:      serverKind(cfg),
		Boot:      boot,
		Log:       log,
		Notifier:  wake,
		ReadGate:  readGateOf(ident),
		// Forgejo #347: the repo-scoped push gate (CheckPush for
		// existing repos, CheckCreateOwner for auto-create) over the
		// same identity service (nil in setup-only → legacy gating).
		PushGate: pushGateOf(ident),
		// Forgejo #240: the pull-only refusal predicate over the same
		// store (nil in setup-only — no push paths exist there).
		MirrorGuard: mirrorGuardOf(st),
		// #210 adoption hints: the same instance apiEnv carries above
		// (nil in setup-only — no create or push paths exist there).
		PlaceholderHints: placeholderHintsOf(apiEnv),
	})
	// Features 01–08 mount here (collab.go chainCollab, 09 §4 touch
	// point 3: one block per package + the per-user SSE mounts that
	// ride the notify handler).
	chainCollab(srv, collab)

	// the SSH key registry backs both the sshd auth lookup and the
	// /api/v1/ssh-keys surface (17_ssh.md §3); setup-only has no store, so
	// the keys surface stays down until a valid config boots
	if apiEnv != nil {
		apiEnv.SSHKeys = srv.SSHKeyRegistry()
	}

	// ---- background loops (§10.4 step 5), gated by server.roles ---------------
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()

	var maintainerDone chan struct{}
	if !setupOnly {
		if len(roles) == 0 || roles["maintain"] {
			m := maintain.NewWalMaintainer(reg, maintain.Options{})
			maintainerDone = make(chan struct{})
			go func() {
				defer close(maintainerDone)
				m.Run(drainCtx) // maintenance stops at phase-1 drain (§12)
			}()
			if collab != nil && collab.mirrorSvc != nil {
				// Forgejo #240 scheduled-sync loop (the follow.go
				// shape: its own cadence, never a maintenance unit,
				// never blocking maintenance). The bucket lease (R1
				// (d)) arbitrates across maintain hosts, so the loop
				// runs on every maintain host; enumeration is the
				// in-memory registry + mirror.json probes (no LIST).
				go collab.mirrorSvc.RunLoop(drainCtx, mirrorLoopInterval, nil)
			}
		}
		prewarmAsync(ctx, engine, cfg.Cache.Prewarm, cfg.Cache.PrewarmParallelism, log)
		go watchdog(ctx, log)
		if notifySvc != nil {
			// Feature 06 background loops (Seam 5): webhook wake-ups +
			// sweeps and the daily retention pass. Same pattern as the
			// events bridge above; stops at drain via ctx.
			go notifySvc.Run(ctx)
		}
	}

	// ---- listener (§10.4 step 6): plain HTTP with h2c ---------------------------
	// TLS termination belongs on the reverse proxy in front of walhub
	// (#165): the server never wraps the listener.
	handler := srv.Handler()
	httpSrv := srv.NewHTTPServer(handler, appCtxer{ctx})

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Error("listen failed", "addr", cfg.Server.Listen, "err", err)
		return exitErr
	}
	ln = srv.BuildListener(ln) // TCP_NODELAY per connection
	log.Info("listening", "addr", cfg.Server.Listen, "version", version())

	// ---- SSH git transport (17_ssh.md): disabled unless server.ssh.listen set --
	if sshSrv, serr := srv.SSH(); serr != nil {
		log.Error("ssh disabled: config error", "err", serr)
	} else if sshSrv != nil {
		go func() {
			if err := sshSrv.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
				log.Error("ssh server stopped", "err", err)
			}
		}()
	}

	serveErr := make(chan error, 2)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	// Loopback IPv6 twin so *.localhost works (§10.4 step 6).
	if twin, ok := loopbackTwin(cfg.Server.Listen); ok {
		go func() {
			if tln, terr := net.Listen("tcp", twin); terr == nil {
				_ = httpSrv.Serve(srv.BuildListener(tln))
			}
		}()
	}

	// ---- two-phase drain on SIGTERM/SIGINT (§12) --------------------------------
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve failed", "err", err)
			return exitErr
		}
		return exitOK
	case <-ctx.Done():
	}

	// Phase 1 (bounded 30 s): maintenance stops; serving + /readyz stay up.
	srv.RunPhase1(drainCtx, ctx)
	// Feature-task drain (10 §B5 + 13 §8): cancel in-flight import
	// leaders AND bodies promptly and refuse new ones. reg.Tasks().Drain()
	// is the registry owner's hook (cancels every running task runCtx —
	// SIGKILLs the import clone, fails its store work); importSvc.Drain()
	// is the service hook (cancels the leader Run wait, gates Begin, arms
	// the commit-point guard that refuses post-drain manifest CAS commits).
	if reg != nil {
		reg.Tasks().Drain()
	}
	if collab != nil && collab.importSvc != nil {
		collab.importSvc.Drain()
	}
	// Notify task leaders (webhooks, fanout) derive their store work
	// from the service drainCtx: Drain cancels them promptly so a
	// wedged store call terminates instead of hanging shutdown
	// (issue #154 — same shape as the #74 import fix above).
	if collab != nil && collab.notifySvc != nil {
		collab.notifySvc.Drain()
	}
	if maintainerDone != nil {
		select {
		case <-maintainerDone:
		case <-time.After(30 * time.Second):
		}
	}
	// Phase 2: readyz flips, new requests refused, in-flight capped.
	srv.RunPhase2(httpSrv)
	cancelDrain()
	if reg != nil {
		reg.Close()
	}
	log.Info("drained; exiting", "version", version())
	return exitOK
}

// newAPI builds the RouteProvider over the api env (nil-safe).
func newAPI(env *api.Env) server.RouteProvider {
	if env == nil {
		return nil
	}
	return server.NewAPIProvider(env)
}

// readGateOf adapts the identity service to the server ReadGate seam
// (nil in setup-only mode → legacy read gating).
func readGateOf(ident *identity.Service) server.ReadGate {
	if ident == nil {
		return nil
	}
	return ident
}

// pushGateOf adapts the identity service to the server PushGate seam
// (Forgejo #347: nil in setup-only mode → legacy host-flag push gating).
// The compiler pins the contract: *identity.Service must satisfy both
// CheckPush and CheckCreateOwner.
func pushGateOf(ident *identity.Service) server.PushGate {
	if ident == nil {
		return nil
	}
	var _ server.PushGate = ident
	return ident
}

// placeholderHintsOf shares the #210 adoption hint set between the api
// create paths and the server push pipeline (nil in setup-only mode →
// no push-path marker ops).
func placeholderHintsOf(env *api.Env) *api.PlaceholderHints {
	if env == nil {
		return nil
	}
	return env.PlaceholderHints
}

// appCtxer adapts the process context to http.Server BaseContext.
type appCtxer struct{ ctx context.Context }

func (a appCtxer) Context() context.Context { return a.ctx }

// watchdog ticks 1 s and warns when a tick is > 2.5 s late (§10.4 step 5).
func watchdog(ctx context.Context, log *slog.Logger) {
	last := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if d := now.Sub(last); d > 2500*time.Millisecond {
				log.Warn("runtime stalled", "gap", d.String())
			}
			last = now
		}
	}
}

// prewarmAsync warms the configured repos with bounded parallelism and gates
// /readyz until done (cache.prewarm_ready_timeout is enforced by readyz).
func prewarmAsync(ctx context.Context, eng *server.WalEngine, repos []string, parallelism int, log *slog.Logger) {
	if len(repos) == 0 || eng == nil {
		return
	}
	if parallelism <= 0 {
		parallelism = 2
	}
	server.SetPrewarmPending(int64(len(repos)))
	go func() {
		sem := make(chan struct{}, parallelism)
		var wg sync.WaitGroup
		var remaining int64 = int64(len(repos))
		for _, id := range repos {
			wg.Add(1)
			sem <- struct{}{}
			go func(repo string) {
				defer wg.Done()
				defer func() { <-sem }()
				rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				rid, err := git.ParseRepoId(repo)
				if err != nil {
					log.Warn("prewarm: invalid repo id", "repo", repo)
					return
				}
				if err := eng.Sync(rctx, rid, wal.LevelFull); err != nil {
					log.Warn("prewarm failed", "repo", repo, "err", err) // never fatal
				}
				remaining--
				server.SetPrewarmPending(remaining)
			}(id)
		}
		wg.Wait()
	}()
}

// ---- helpers ---------------------------------------------------------------------

// roleSet normalizes server.roles (empty = all roles).
func roleSet(roles []string) map[string]bool {
	out := map[string]bool{}
	for _, r := range roles {
		out[r] = true
	}
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func instanceID(cfg *config.Config) string {
	if cfg.Maintenance.Host != "" {
		return cfg.Maintenance.Host
	}
	return hostname()
}

func serverKind(cfg *config.Config) server.ServerKind {
	if cfg.Cache.Mode == "disk" || cfg.Maintenance.Disk == "ssd" {
		return server.KindSSD
	}
	if cfg.Cache.Mode == "budget" {
		return server.KindServerless
	}
	return server.KindDev
}

// mustWarnings surfaces validation warnings at boot (§5 rule 1 and friends).
func mustWarnings(cfg *config.Config) []string {
	warnings, errs := config.Validate(cfg)
	out := append([]string{}, warnings...)
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}

// logSetupBanner emits the zero-config first-run banner (§2.3).
func logSetupBanner(cfg *config.Config) {
	slog.Warn("zero-config first run: auth.mode=none on " + cfg.Server.Listen +
		"; anyone who can reach this port can read and write every repository — configure server.auth and restart; the web UI shows a persistent banner linking /setup until a config file exists")
}

// openStore builds the object store per store.backend (D4).
func openStore(cfg *config.Config, dataDir string) (store.ObjectStore, error) {
	switch cfg.Store.Backend {
	case "memory":
		return store.NewMemory(), nil
	case "filesystem":
		root := cfg.Store.Root
		if root == "" {
			root = filepath.Join(dataDir, "store")
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("create store root %s: %w", root, err)
		}
		fsStore, err := store.NewFilesystemRoot(root, cfg.Store.GCS.BulkConcurrency)
		if err != nil {
			return nil, err
		}
		return fsStore, nil
	case "s3":
		return store.NewS3(&cfg.Store)
	case "gcs":
		return nil, fmt.Errorf("store.backend = %q: the GCS backend is not part of this build; use filesystem, s3, or memory", cfg.Store.Backend)
	default:
		return nil, fmt.Errorf("unknown store.backend %q", cfg.Store.Backend)
	}
}

// loopbackTwin reports the IPv6 twin listener address for a 127.0.0.1 bind.
func loopbackTwin(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host != "127.0.0.1" {
		return "", false
	}
	return net.JoinHostPort("::1", port), true
}

// newLogger builds the slog logger per telemetry.log_format / log_filter
// (RUST_LOG / WALHUB_LOG win when set).
func newLogger(cfg *config.Config) *slog.Logger {
	filter := cfg.Telemetry.LogFilter
	if v := os.Getenv("WALHUB_LOG"); v != "" {
		filter = v
	} else if v := os.Getenv("RUST_LOG"); v != "" {
		filter = v
	}
	level := slog.LevelInfo
	if strings.Contains(strings.ToLower(filter), "debug") || strings.Contains(strings.ToLower(filter), "trace") {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Telemetry.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// applyPortOverride re-applies the §3.3 PORT lockstep to a config built
// outside the loader (the setup-only fallback cfg).
func applyPortOverride(cfg *config.Config) {
	p := os.Getenv("PORT")
	if p == "" {
		return
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return
	}
	host, _, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		return
	}
	cfg.Server.Listen = net.JoinHostPort(host, strconv.Itoa(port))
}

// ---- RepoRegistry adapter (07_api.md §8: answered from the STORE) --------------
//
// Owners/Repos are MANIFEST-gated: a prefix without repos/<o>/<r>/manifest.pb
// is not a repository (issue #200). Stale prefixes linger after Delete: the
// sweep removes every listed object, but the filesystem CAS sidecars
// (<name>.lock) persist by design — invisible to List, yet their directory
// keeps the name behind ListPrefixes — and a fork-provisioned prefix
// (fork.json written before the child manifest lands, #150) is likewise
// unborn. Gating on the manifest keeps both out of the listings on every
// backend; Exists already gates the same way, so re-create/re-import after a
// delete sees a clean name.
type repoRegistry struct {
	reg *wal.Registry
	st  store.ObjectStore
}

func (r *repoRegistry) Owners(ctx context.Context) ([]string, error) {
	owners, err := listOwners(ctx, r.st)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, owner := range owners {
		repos, rerr := r.liveRepos(ctx, owner)
		if rerr != nil {
			return nil, rerr
		}
		if len(repos) > 0 {
			out = append(out, owner)
		}
	}
	return out, nil
}

func (r *repoRegistry) Repos(ctx context.Context, owner string) ([]string, error) {
	return r.liveRepos(ctx, owner)
}

// OwnerRepoCounts answers the api.RepoRegistry per-owner live-repo counts
// (Forgejo #307 — the owners/detailed repo_count rail). It walks exactly
// what Owners walks — one liveRepos pass per owner, same manifest-gated
// ghost rule, same trip profile — and keeps the lengths Owners discards,
// so the detailed endpoint pays zero added store trips for the counts.
// Owners with zero live repos are absent, never zero-valued.
//
// ### Concurrency
// Hazard: none new — the loop is sequential like Owners (no shared mutable
// state, no lock); parallelism lives inside each liveRepos call, unchanged.
func (r *repoRegistry) OwnerRepoCounts(ctx context.Context) (map[string]int, error) {
	owners, err := listOwners(ctx, r.st)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, owner := range owners {
		repos, rerr := r.liveRepos(ctx, owner)
		if rerr != nil {
			return nil, rerr
		}
		if len(repos) > 0 {
			out[owner] = len(repos)
		}
	}
	return out, nil
}

// liveRepos lists the manifest-backed repos under owner: the raw prefix
// listing minus deleted-repo ghosts (manifest.pb swept, sidecar litter
// behind) and unborn fork-provisioned prefixes (fork.json, manifest not yet
// written — #150, which the row-level tolerateMissing still covers as a
// race). Manifest Heads run in parallel (limit 8, mirroring
// wal.refreshList); a missing/unreadable manifest drops the name — the same
// fail-closed choice as refreshList, so a transient store error hides a repo
// from one listing refresh rather than resurrecting a deleted one.
//
// ### Concurrency
// Hazard: parallel Head goroutines appending to one slice. Avoidance: a
// single mutex guards the collect; no lock is held across a store call (the
// Head happens before the lock). Output is sorted after the join.
func (r *repoRegistry) liveRepos(ctx context.Context, owner string) ([]string, error) {
	names, err := listRepos(ctx, r.st, owner)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []string{}, nil
	}
	g, gctx := store.WithContext(ctx)
	g.SetLimit(8)
	var mu sync.Mutex
	live := make([]string, 0, len(names))
	for _, name := range names {
		name := name
		g.Go(func() error {
			meta, herr := r.st.Head(gctx, "repos/"+owner+"/"+name+"/"+store.Manifest)
			if herr != nil || meta == nil {
				return nil // absent/unreadable manifest = not a repo
			}
			mu.Lock()
			live = append(live, name)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(live)
	return live, nil
}

func (r *repoRegistry) Exists(ctx context.Context, id git.RepoId) (bool, error) {
	meta, err := r.st.Head(ctx, id.StorePrefix()+store.Manifest)
	if err != nil || meta == nil {
		return false, nil //nolint:nilerr // absent manifest = not registered
	}
	return true, nil
}

func (r *repoRegistry) Create(ctx context.Context, id git.RepoId, format git.ObjectFormat) error {
	if _, err := r.reg.Create(ctx, id.String(), format); err != nil {
		var we *wal.WalError
		if errors.As(err, &we) && we.Kind == wal.WalErrAlreadyExists {
			return fmt.Errorf("%w: %s", api.ErrExists, id.String())
		}
		return err
	}
	return nil
}

func (r *repoRegistry) Delete(ctx context.Context, id git.RepoId) error {
	_, err := r.reg.Delete(ctx, id.String())
	return err
}

// ---- api.Tasks adapter (07_api.md §12: ops + task table) ------------------------

// opsTasks binds the api.Tasks seam onto wal.TaskTable and the engine ops.
type opsTasks struct {
	reg   *wal.Registry
	eng   *server.WalEngine
	ident *identity.Service
}

func (t *opsTasks) Ops() []api.OpSpec {
	ops := []api.OpSpec{
		{Op: "sync"},
		{Op: "checkpoint", Params: []api.OpParam{{Name: "trigger"}}},
	}
	if t.ident != nil {
		// Seam 5: the access-bootstrap migration (docs/features/01 §10).
		ops = append(ops, api.OpSpec{Op: "access-bootstrap"})
	}
	return ops
}

func (t *opsTasks) List(ctx context.Context, id git.RepoId) ([]api.TaskRecord, []api.TaskRecord, error) {
	recent := []api.TaskRecord{}
	for _, rec := range t.reg.Tasks().List(id.String()) {
		recent = append(recent, api.TaskRecordFromWal(*rec))
	}
	return nil, recent, nil
}

func (t *opsTasks) Get(ctx context.Context, id git.RepoId, taskID string) (api.TaskRecord, bool, error) {
	rec := t.reg.Tasks().Get(taskID)
	if rec == nil {
		return api.TaskRecord{}, false, nil
	}
	return api.TaskRecordFromWal(*rec), true, nil
}

// Begin starts (or joins) an op; the SSE stream replays the per-repo progress
// broadcast and follows it live until the task record finishes.
func (t *opsTasks) Begin(ctx context.Context, id git.RepoId, op string, params map[string]string) (api.TaskStream, error) {
	h, err := t.reg.Open(ctx, id.String())
	if err != nil {
		return api.TaskStream{}, err
	}
	fn := t.opFn(id, op, params)
	if fn == nil {
		return api.TaskStream{}, fmt.Errorf("op %q is not available on this instance", op)
	}
	subID, updates, replay := h.Progress().Subscribe()
	recCh := make(chan api.TaskDone, 1)
	go func() {
		defer h.Progress().Unsubscribe(subID)
		rec, runErr := t.reg.Tasks().Run(context.WithoutCancel(ctx), id.String(), op, params, fn)
		done := api.TaskDone{}
		if runErr != nil {
			done.Err = &api.TaskErr{Status: http.StatusInternalServerError, Message: runErr.Error()}
		}
		if rec != nil {
			done.Record = api.TaskRecordFromWal(*rec)
		}
		recCh <- done
	}()
	return api.TaskStream{Replay: replayProgress(replay), Updates: convertProgress(updates), Done: recCh}, nil
}

// convertProgress bridges wal.Progress onto the wire type on a new channel.
func convertProgress(in <-chan wal.Progress) <-chan api.Progress {
	out := make(chan api.Progress)
	go func() {
		defer close(out)
		for p := range in {
			out <- api.ProgressFromWal(p)
		}
	}()
	return out
}

func (t *opsTasks) Attach(ctx context.Context, id git.RepoId, taskID string) (api.TaskStream, bool, error) {
	rec := t.reg.Tasks().Get(taskID)
	if rec == nil {
		return api.TaskStream{}, false, nil
	}
	h := t.reg.Get(id.String())
	if h == nil {
		return api.TaskStream{}, false, nil
	}
	if rec.Finished != "" {
		done := api.TaskDone{Record: api.TaskRecordFromWal(*rec)}
		recCh := make(chan api.TaskDone, 1)
		recCh <- done
		return api.TaskStream{Record: done.Record, Done: recCh}, true, nil
	}
	subID, updates, replay := h.Progress().Subscribe()
	recCh := make(chan api.TaskDone, 1)
	ticker := time.NewTicker(500 * time.Millisecond)
	go func() {
		defer h.Progress().Unsubscribe(subID)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if cur := t.reg.Tasks().Get(taskID); cur != nil && cur.Finished != "" {
					done := api.TaskDone{Record: api.TaskRecordFromWal(*cur)}
					recCh <- done
					return
				}
			}
		}
	}()
	return api.TaskStream{Record: api.TaskRecordFromWal(*rec), Replay: replayProgress(replay), Updates: convertProgress(updates), Done: recCh}, true, nil
}

// opFn maps an op name onto its engine action (§12.2 table subset).
func (t *opsTasks) opFn(id git.RepoId, op string, params map[string]string) func(ctx context.Context, task *wal.Task) error {
	switch op {
	case "sync":
		return func(ctx context.Context, task *wal.Task) error {
			return t.eng.Sync(ctx, id, wal.LevelServe)
		}
	case "checkpoint":
		return func(ctx context.Context, task *wal.Task) error {
			h := t.reg.Get(id.String())
			if h == nil {
				return fmt.Errorf("repo %s not open", id.String())
			}
			trigger := params["trigger"]
			if trigger == "" {
				trigger = string(wal.TriggerManual)
			}
			return h.WriteCheckpoint(ctx, wal.CheckpointTrigger(trigger))
		}
	case "access-bootstrap":
		if t.ident == nil {
			return nil
		}
		return func(ctx context.Context, task *wal.Task) error {
			created, err := t.ident.BootstrapRepo(ctx, id.Owner, id.Name)
			if err != nil {
				return err
			}
			task.Notice(bootstrapSummary(id.String(), created))
			return nil
		}
	default:
		return nil
	}
}

// bootstrapSummary narrates the access-bootstrap outcome (Seam 5 task).
func bootstrapSummary(repo string, created bool) string {
	if created {
		return "access-bootstrap " + repo + ": materialized"
	}
	return "access-bootstrap " + repo + ": already present"
}

func replayProgress(in []wal.Progress) []api.Progress {
	out := make([]api.Progress, 0, len(in))
	for _, p := range in {
		out = append(out, api.ProgressFromWal(p))
	}
	return out
}
