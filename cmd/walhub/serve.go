// serve.go — `walhub serve` (the default): the §10.4 startup order of
// 06_server_http.md — bootstrap decision tree, store by backend, registry,
// engine, api env, chi server, loops gated by server.roles, prewarm,
// watchdog, TLS/h2c listener, and the two-phase drain on SIGTERM/SIGINT.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/events"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/issues"
	"git.packden.us/crueber/walhub/internal/maintain"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
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
	var ident *identity.Service
	var identHandler *identity.Handler
	var issuesSvc *issues.Service
	var issuesHandler *issues.Handler
	var pullsSvc *pulls.Service
	var pullsHandler *pulls.Handler
	if !setupOnly {
		wal.SetWarnLogger(func(format string, args ...any) { log.Warn(fmt.Sprintf(format, args...)) })
		reg = wal.NewRegistry(ctx, st, cfg)
		engine = server.NewWalEngine(reg, cfg)
		apiEnv = api.NewEnv(st, &repoRegistry{reg: reg, st: st}, cfg, engine, version(), hostname())
		apiEnv.Tasks = &opsTasks{reg: reg, eng: engine}
		apiEnv.Instance = reg.InstanceID()
		// Wave A identity (docs/features/01): the access.json/org/team
		// surface (Seam 1, both lanes), the require_read gate (Seam 3
		// expansion wired into dry-run via GroupExpander), and the
		// access-bootstrap op (Seam 5).
		ident = identity.New(st, cfg)
		identHandler = &identity.Handler{Svc: ident}
		apiEnv.Access = ident
		apiEnv.GroupExpander = ident.PolicyExpander()
		// Wave B issues (docs/features/02): the thread/event/label/
		// milestone surface (Seam 1, both lanes) over the P6 roles owned
		// by identity. Notifications emit through the 02 §10 seam —
		// nil until internal/notify lands (best-effort synchronous
		// fan-out contract, P8).
		issuesSvc = issues.New(st, ident)
		issuesHandler = &issues.Handler{Svc: issuesSvc}
		if ot, ok := apiEnv.Tasks.(*opsTasks); ok {
			ot.ident = ident
		}
		// Wave C1 pulls (docs/features/03): PR threads over the shared
		// numbering/thread/index family, pr.json sidecars, the stamped
		// mergeable.json cache, the pull-merge/pull-mergeable/pull-fork
		// tasks, and the pulls event sink. Ref publishes funnel through
		// the WAL publish path (never force); the merge task calls 02's
		// ApplyClosingReferences seam via issuesSvc.
		pullsSvc, pullsHandler = newPullsService(st, ident, issuesSvc, reg, cfg.Git.Binary)
	}

	// ---- events bridge before server.New (§10.4 order: AppState then loops) --
	roles := roleSet(cfg.Server.Roles)
	eventsRole := len(roles) == 0 || roles["events"]
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
		TLSOn:     cfg.Server.TLS.Mode != "off",
		Boot:      boot,
		Log:       log,
		Notifier:  wake,
		ReadGate:  readGateOf(ident),
	})
	if identHandler != nil {
		// Chain the identity surface in front of the core api mux (Seam 1);
		// authentication resolves through the server chain (Seam 2).
		identHandler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return srv.Auth().Authenticate(r, cfg)
		}
		srv.ChainExtra(identHandler)
	}
	if issuesHandler != nil {
		// Chain the issues surface in front of the core api mux (Seam 1);
		// authentication resolves through the server chain (Seam 2).
		issuesHandler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return srv.Auth().Authenticate(r, cfg)
		}
		srv.ChainExtra(issuesHandler)
	}
	if pullsHandler != nil {
		chainPulls(srv, pullsHandler)
	}

	// the SSH key registry backs both the sshd auth lookup and the
	// /api/v1/ssh-keys surface (17_ssh.md §3); setup-only has no store, so
	// the keys surface stays down until a valid config boots
	if apiEnv != nil {
		apiEnv.SSHKeys = srv.SSHKeyRegistry()
	}

	// ---- background loops (§10.4 step 6), gated by server.roles ---------------
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
		}
		prewarmAsync(ctx, engine, cfg.Cache.Prewarm, cfg.Cache.PrewarmParallelism, log)
		go watchdog(ctx, log)
	}

	// ---- listener (§10.4 step 7): TLS/h2c per config ---------------------------
	handler := srv.Handler()
	httpSrv := srv.NewHTTPServer(handler, appCtxer{ctx})

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Error("listen failed", "addr", cfg.Server.Listen, "err", err)
		return exitErr
	}
	ln = srv.BuildListener(ln) // TCP_NODELAY per connection

	if tlsCfg := tlsConfigFor(srv, cfg); tlsCfg != nil {
		http2.ConfigureServer(httpSrv, &http2.Server{}) // ALPN h2 over TLS
		ln = tls.NewListener(ln, tlsCfg)
		log.Info("listening (tls)", "addr", cfg.Server.Listen, "version", version())
	} else {
		log.Info("listening", "addr", cfg.Server.Listen, "version", version())
	}

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
	// Loopback IPv6 twin so *.localhost works (§10.4 step 7).
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

// appCtxer adapts the process context to http.Server BaseContext.
type appCtxer struct{ ctx context.Context }

func (a appCtxer) Context() context.Context { return a.ctx }

// watchdog ticks 1 s and warns when a tick is > 2.5 s late (§10.4 step 6).
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

// tlsConfigFor returns the TLS config for files/self_signed modes (nil = off).
func tlsConfigFor(srv *server.Server, cfg *config.Config) *tls.Config {
	switch cfg.Server.TLS.Mode {
	case "self_signed":
		if err := srv.EnsureSelfSigned(); err != nil {
			slog.Error("self-signed generation failed", "err", err)
			return nil
		}
	case "files":
		// cert/key paths validated at load.
	default:
		return nil
	}
	tc, err := srv.TLSServerConfig()
	if err != nil {
		slog.Error("tls load failed", "err", err)
		return nil
	}
	return tc
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

type repoRegistry struct {
	reg *wal.Registry
	st  store.ObjectStore
}

func (r *repoRegistry) Owners(ctx context.Context) ([]string, error) {
	out := []string{}
	err := r.st.ListPrefixes(ctx, "repos/", func(m string) error {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(m, "repos/"), "/"))
		return nil
	})
	return out, err
}

func (r *repoRegistry) Repos(ctx context.Context, owner string) ([]string, error) {
	out := []string{}
	prefix := "repos/" + owner + "/"
	err := r.st.ListPrefixes(ctx, prefix, func(m string) error {
		name := strings.TrimPrefix(m, prefix)
		if strings.HasSuffix(name, "/") {
			out = append(out, strings.TrimSuffix(name, "/"))
		}
		return nil
	})
	return out, err
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
