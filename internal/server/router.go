package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Handler builds the one http.Handler chain (§2.2): the explicit ordered
// middleware slice applied through chi's Use — all before any route
// registration — then the §3 route tree.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	for _, name := range middlewareOrder { // outermost first; MUST precede registration
		f := s.middlewareByName(name)
		if f != nil {
			r.Use(f)
		}
	}
	s.mount(r)
	return r
}

// apiServe delegates to the api seam with the request principal resolved
// and injected (06 §8): invalid credentials get a real 401 here (the seam
// handlers then see the same principal the git paths resolve). Anonymous
// requests pass through untouched, preserving legacy read behavior.
func (s *Server) apiServe(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	p = s.authSvc.identityForward(r, p)
	s.maybeRefreshSession(w, r, p)
	s.api.Serve(w, injectPrincipal(r, p))
}

// gated wraps a handler for the gated group (requireAuth = read).
func (s *Server) gated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, aerr := s.authSvc.Authenticate(r, s.cfg)
		switch {
		case aerr != nil:
			s.authFailure(w, r, aerr)
		case p.Anonymous && !s.cfg.Server.Auth.AnonymousRead:
			s.authFailure(w, r, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		default:
			s.maybeRefreshSession(w, r, p)
			h(w, injectPrincipal(r, p))
		}
	}
}

// mount registers the §3.1 chi route tree. SETUP-ONLY MODE (§3.4) branches at
// the top: only the §3.4 subset is registered plus a 503 fallback.
func (s *Server) mount(r chi.Router) {
	if s.boot.InSetupOnly() {
		s.mountSetupOnly(r)
		return
	}

	// Health & SDK (open).
	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/repos.js", s.sdkReposJS)
	r.Get("/services/public/install.sh", s.installSh)
	r.Get("/services/public/ca.pem", s.caPem)

	// Setup API mounted BEFORE the /api/v1 catch (§3.1).
	r.Mount("/api/v1/setup", s.setupAPIRouter())

	// JSON API + non-repo lanes (§3.1): one handler group with compress.
	// api.Mount is an opaque mux (ServeMux full-path patterns), so the lane
	// handler is wrapped with the compress factory rather than a chi Group.
	if s.api != nil {
		// Compress stays scoped to the web lanes: the api mux is opaque, so
		// the compress factory wraps the lane handler directly (§2.2 #9).
		apiLane := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.apiServe(w, r)
		}))
		r.HandleFunc("/api", apiLane.ServeHTTP)
		r.HandleFunc("/api/*", apiLane.ServeHTTP)
		r.HandleFunc("/api-browser", apiLane.ServeHTTP)
		r.HandleFunc("/api-browser/*", apiLane.ServeHTTP)
		r.HandleFunc("/services/api", apiLane.ServeHTTP)
		r.HandleFunc("/services/api/*", apiLane.ServeHTTP)
	} else {
		r.HandleFunc("/api*", func(w http.ResponseWriter, r *http.Request) {
			plainStatus(w, http.StatusServiceUnavailable, "api not wired")
		})
		r.HandleFunc("/api/*", func(w http.ResponseWriter, r *http.Request) {
			plainStatus(w, http.StatusServiceUnavailable, "api not wired")
		})
		r.HandleFunc("/api-browser*", func(w http.ResponseWriter, r *http.Request) {
			plainStatus(w, http.StatusServiceUnavailable, "api not wired")
		})
	}

	// Browser OIDC flow (§8.6).
	r.Mount("/_auth", s.authFlow())

	r.Post("/_events/notify", s.eventsNotify) // handler-authenticated (09_events.md)

	// SPA assets (D-WEB-6): static, content-hashed, no secrets — auth is the
	// API's job; the shell must load everywhere so /setup can render its
	// access-restricted card. gzip applies to the text assets.
	r.Route("/_ui", func(g chi.Router) {
		g.Handle("/*", s.compress(http.HandlerFunc(s.serveUIAssets)))
	})
	r.Get("/services/setup.json", s.gated(s.setupJSON))
	r.Get("/metrics", s.gated(s.metricsHandler))

	// Setup page (§3.4) — a SPA route; the shell loads everywhere.
	r.Get("/setup", s.setupUI)

	// SPA shell.
	r.Get("/", s.gated(s.spaHome))

	r.NotFound(notFound)                 // deliberate 404, plain text
	r.MethodNotAllowed(methodNotAllowed) // 405 + Allow

	r.HandleFunc("/*", s.repoDispatch) // EVERYTHING repo-scoped (§3.2)
}

// mountSetupOnly registers the §3.4 setup-only subset: /setup*, /api/v1/setup*,
// /healthz, /readyz, /services/public/*; everything else 503 plain text with
// a pointer to /setup.
func (s *Server) mountSetupOnly(r chi.Router) {
	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Mount("/api/v1/setup", s.setupAPIRouter())
	r.Get("/setup", s.setupUI) // SPA shell; assets below are open
	r.Route("/_ui", func(g chi.Router) {
		g.Handle("/*", s.compress(http.HandlerFunc(s.serveUIAssets)))
	})
	r.Get("/services/public/install.sh", s.installSh)
	r.Get("/services/public/ca.pem", s.caPem)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		plainStatus(w, http.StatusServiceUnavailable,
			"walgit: configuration is invalid — open /setup to fix it")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "15")
		plainStatus(w, http.StatusServiceUnavailable,
			"walgit: configuration is invalid — open /setup to fix it")
	})
}

// setupAPIRouter returns the internal/setup HTTP surface (§3.4) — the server
// owns it until internal/setup lands (deviation noted).
func (s *Server) setupAPIRouter() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/setup", s.setupGet)
	m.HandleFunc("POST /api/v1/setup/test", s.setupTest)
	m.HandleFunc("PUT /api/v1/setup", s.setupPut)
	m.HandleFunc("POST /api/v1/setup/auth/test", s.setupAuthTest)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		plainStatus(w, http.StatusNotFound, "not found")
	})
	return m
}

// authFlow builds the /_auth/* mux (§8.6).
func (s *Server) authFlow() http.Handler {
	// URL.Path is NOT stripped under chi Mount, so the mux needs full paths.
	m := http.NewServeMux()
	m.HandleFunc("GET /_auth/login", s.authLogin)
	m.HandleFunc("GET /_auth/callback", s.authCallback)
	m.HandleFunc("GET /_auth/claimed", s.authClaimed)
	m.HandleFunc("GET /_auth/logout", s.authLogout)
	m.HandleFunc("GET /_auth/me", s.authMe)
	m.HandleFunc("GET /_auth/check", s.authCheck)
	m.HandleFunc("GET /_auth/tokens", s.authTokensPage)
	m.HandleFunc("POST /_auth/tokens", s.authTokensMint)
	return m
}

// repoDispatch is the §3.2 wildcard: the repository prefix is the only
// routing key; everything after is dispatched by hand, which is what makes
// `.git`-everywhere work (no single chi pattern can express the optional
// suffix). It is also the last-resort 404 for non-repo-shaped junk.
func (s *Server) repoDispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, "/") { // paranoia
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) < 2 { // "/{owner}" → UI page route (gated, GET/HEAD only)
		owner, err := decodeSeg(segs[0])
		if err != nil || owner == "" || !validOwner(owner) {
			plainStatus(w, http.StatusNotFound, "not found")
			return
		}
		if !isUIPageMethod(r) {
			w.Header().Set("Allow", "GET, HEAD")
			plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.gated(s.ownerPage)(w, r)
		return
	}
	owner, rerr := decodeSeg(segs[0])
	repoSeg, rerr2 := decodeSeg(segs[1])
	if rerr != nil || rerr2 != nil {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	hadGit := false
	if strings.HasSuffix(repoSeg, ".git") { // accepted everywhere and stripped
		repoSeg = strings.TrimSuffix(repoSeg, ".git")
		hadGit = true
	}
	id, perr := git.ParseRepoId(owner + "/" + repoSeg)
	if perr != nil {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	sub := segs[2:]
	for i := range sub {
		d, derr := decodeSeg(sub[i])
		if derr != nil {
			plainStatus(w, http.StatusBadRequest, "invalid path encoding")
			return
		}
		sub[i] = d
	}
	// Route the lanes to the api seam first (§3.3: both lanes, same handlers).
	if len(sub) > 0 && (sub[0] == "api" || sub[0] == "api-browser") && s.api != nil {
		s.apiServe(w, r)
		return
	}
	// Repo lifecycle on the bare repo root: PUT create / DELETE delete.
	if len(sub) == 0 && (r.Method == http.MethodPut || r.Method == http.MethodDelete) && s.api != nil {
		s.apiServe(w, r)
		return
	}

	gitish := isGitClient(r.UserAgent())
	// §4.3: placement/drain gates run before any sync work.
	if s.drainGate(w, gitish, s.pktErrFor(w, r)) {
		return
	}

	switch {
	case len(sub) == 0:
		// UI page route /{owner}/{repo} (gated, GET/HEAD only; PUT/DELETE on
		// the bare root went to the api seam above).
		if !isUIPageMethod(r) {
			w.Header().Set("Allow", "GET, HEAD")
			plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.gated(s.repoPage(id))(w, r)
	case sub[0] == "info" && len(sub) >= 2 && sub[1] == "refs":
		s.gitInfoRefs(w, r, id)
	case sub[0] == "git-upload-pack":
		s.gitService(w, r, id, git.ServiceUploadPack, hadGit)
	case sub[0] == "git-receive-pack":
		s.gitService(w, r, id, git.ServiceReceivePack, hadGit)
	case sub[0] == "info" && len(sub) >= 4 && sub[1] == "lfs":
		s.lfsDispatch(w, r, id, sub[2:])
	case sub[0] == "bundles":
		s.bundlesDispatch(w, r, id, sub[1:], hadGit)
	case len(sub) == 1 && sub[0] == "api" || len(sub) == 1 && sub[0] == "api-browser":
		// Lane root GET is handled above; fall through to UI check.
		s.gated(s.repoPage(id))(w, r)
	case uiPageRoute(sub[0]):
		if !isUIPageMethod(r) {
			w.Header().Set("Allow", "GET, HEAD")
			plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.gated(s.repoPage(id))(w, r)
	default:
		plainStatus(w, http.StatusNotFound, "not found")
	}
}

// isUIPageMethod: UI pages answer GET/HEAD only.
func isUIPageMethod(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// uiPageRoute reports the /{o}/{r}/<page> UI routes (§3.3 gated list).
func uiPageRoute(sub0 string) bool {
	switch sub0 {
	case "tree", "blob", "commits", "commit", "wal", "settings":
		return true
	}
	return false
}

// validOwner is the owner charset pre-check (cheap reject before parse).
func validOwner(s string) bool { _, err := git.ParseRepoId(s + "/x"); return err == nil }

// decodeSeg decodes one path segment.
func decodeSeg(s string) (string, error) { return url.PathUnescape(s) }

func notFound(w http.ResponseWriter, r *http.Request) {
	plainStatus(w, http.StatusNotFound, "not found")
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
	plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
}
