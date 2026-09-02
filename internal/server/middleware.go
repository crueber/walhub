package server

import (
	"compress/gzip"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

type (
	ctxRequestIDKey struct{}
	ctxTraceIDKey   struct{}
	ctxPrincipalKey struct{}
	ctxLogKey       struct{}
)

// RequestIDOf returns the request id from the context (§2.2 #1).
func RequestIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxRequestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// TraceIDOf returns the parsed trace id (X-Cloud-Trace-Context | traceparent).
func TraceIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxTraceIDKey{}).(string); ok {
		return v
	}
	return ""
}

// traceIDFrom parses the §2.2 #1 trace headers.
func traceIDFrom(r *http.Request) string {
	if v := r.Header.Get("X-Cloud-Trace-Context"); v != "" {
		if i := strings.IndexByte(v, '/'); i > 0 {
			return v[:i]
		}
		return v
	}
	if v := r.Header.Get("traceparent"); v != "" {
		parts := strings.Split(v, "-")
		if len(parts) >= 2 && len(parts[1]) == 32 {
			return parts[1]
		}
	}
	return ""
}

// ReqLog returns the request-scoped logger (the http.request record).
func ReqLog(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(ctxLogKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// middlewareOrder is the FIXED ordered list, outermost first (§2.2). No
// timeout layer and no body-limit layer exist (§20.4 note).
var middlewareOrder = []string{
	"requestID",
	"canonicalBrowserHost",
	"hostFromAuthority",
	"serverHeaders",
	"recoverPanic",
	"cors",
	"refreshSession",
}

// middlewareByName resolves one factory per name — an explicit, reviewable map.
func (s *Server) middlewareByName(name string) func(http.Handler) http.Handler {
	switch name {
	case "requestID":
		return s.requestID
	case "canonicalBrowserHost":
		return s.canonicalBrowserHost
	case "hostFromAuthority":
		return s.hostFromAuthority
	case "serverHeaders":
		return s.serverHeaders
	case "recoverPanic":
		return s.recoverPanic
	case "cors":
		return s.cors
	case "refreshSession":
		return s.refreshSession
	}
	return nil
}

// --- response writer wrappers -------------------------------------------------

// recorder wraps http.ResponseWriter: status, byte counts, a late-header hook
// (serverHeaders) and a write-completion hook (the inflight decrement).
type recorder struct {
	http.ResponseWriter
	status   int
	bytes    int64
	onHeader func(status int)
	onDone   func()
	doneOne  sync.Once
}

func (rw *recorder) WriteHeader(status int) {
	if rw.status != 0 {
		return
	}
	rw.status = status
	if rw.onHeader != nil {
		rw.onHeader(status)
	}
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *recorder) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += int64(n)
	return n, err
}

func (rw *recorder) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish releases the inflight slot exactly once.
func (rw *recorder) finish() { rw.doneOne.Do(rw.releaseFn) }

func (rw *recorder) releaseFn() {
	if rw.onDone != nil {
		rw.onDone()
	}
}

// touched reports whether anything was written.
func (rw *recorder) touched() bool { return rw.status != 0 }

// --- #1 requestID ---------------------------------------------------------------

// requestID honors inbound X-Request-Id else mints a random 16-byte hex UUID,
// echoes it, starts the http.request log record, and owns the inflight gauge:
// increment at entry, decrement on the write-completion hook (not a
// pre-handler defer — a leaked SSE connection must keep its slot until its
// terminal event) (§2.2 #1).
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id) // echo honored, echo minted
		trace := traceIDFrom(r)
		rw := &recorder{ResponseWriter: w}
		s.inflight.n.Add(1)
		s.metrics.Gauge("walgit_http_inflight", "in-flight HTTP requests").Set(s.inflight.N())
		rw.onDone = func() { s.inflight.n.Add(-1) }
		if s.inflight.OverCap() {
			s.log.Warn("inflight over advisory cap", "count", s.inflight.N(), "request_id", id)
		}
		start := s.Now()
		ctx := context.WithValue(r.Context(), ctxRequestIDKey{}, id)
		if trace != "" {
			ctx = context.WithValue(ctx, ctxTraceIDKey{}, trace)
		}
		reqLog := s.log.With(
			"request_id", id, "method", r.Method, "path", r.URL.Path,
			"user_agent", truncate(r.UserAgent(), 200), "trace_id", trace,
		)
		ctx = context.WithValue(ctx, ctxLogKey{}, reqLog)
		next.ServeHTTP(rw, r.WithContext(ctx))
		rw.finish()
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		reqLog.Info("http.request", "status", status, "bytes_out", rw.bytes,
			"elapsed_ms", s.Now().Sub(start).Milliseconds())
	})
}

// --- #2 canonicalBrowserHost ----------------------------------------------------

// browserLooks reports the browser test: Accept contains text/html, OR
// Sec-Fetch-Dest: document, OR UA contains Mozilla (§2.2 #2).
func browserLooks(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html") ||
		r.Header.Get("Sec-Fetch-Dest") == "document" ||
		strings.Contains(r.Header.Get("User-Agent"), "Mozilla")
}

// canonicalSkip are the paths never redirected (§2.2 #2).
var canonicalSkip = []string{"/_auth/", "/healthz", "/readyz", "/services/public"}

// canonicalBrowserHost 302s loopback browser-looking GET/HEAD to
// walgit.localhost[:port], same path+query, scheme https when TLS is on else
// http. Git/curl clients fail the browser test and never redirect.
func (s *Server) canonicalBrowserHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
			!browserLooks(r) || !isLoopbackHost(hostOnly(r.Host)) ||
			strings.HasPrefix(r.Host, "walgit.") {
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range canonicalSkip {
			if strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		scheme := "http"
		if s.tlsOn {
			scheme = "https"
		}
		w.Header().Set("Location", scheme+"://"+canonicalHost(r.Host)+r.URL.RequestURI())
		w.WriteHeader(http.StatusFound)
	})
}

// hostOnly strips the port from a host header value.
func hostOnly(host string) string {
	if h, _, err := netSplit(host); err == nil {
		return h
	}
	return host
}

// --- #3 hostFromAuthority --------------------------------------------------------

// hostFromAuthority copies :authority (net/http puts it in URL.Host on HTTP/2)
// into r.Host when empty — cosmetic normalization for downstream host checks.
func (s *Server) hostFromAuthority(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && r.Host == "" && r.URL.Host != "" {
			r.Host = r.URL.Host
		}
		next.ServeHTTP(w, r)
	})
}

// --- #4 serverHeaders -------------------------------------------------------------

// serverHeaders stamps `Server:`/`X-Walgit-Server:` on every response — errors,
// SSE, static — before WriteHeader via a wrapper so late writes still land.
func (s *Server) serverHeaders(next http.Handler) http.Handler {
	val := s.serverHeaderValue()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recorder{ResponseWriter: w}
		rw.onHeader = func(status int) {
			h := rw.Header()
			if h.Get("Server") == "" {
				h.Set("Server", val)
			}
			if h.Get("X-Walgit-Server") == "" {
				h.Set("X-Walgit-Server", val)
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// --- #5 recoverPanic --------------------------------------------------------------

// recoverPanic logs with request_id and answers 500 plain text "internal
// error" (request_id included). It never kills the process and runs inside
// the inflight-wrapped writer, so the decrement is not swallowed.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "request_id", RequestIDOf(r),
					"path", r.URL.Path, "panic", rec)
				if rw, ok := w.(*recorder); !ok || !rw.touched() {
					plainStatus(w, http.StatusInternalServerError,
						"internal error (request_id "+RequestIDOf(r)+")")
				} else {
					_, _ = w.Write([]byte("internal error (request_id " + RequestIDOf(r) + ")"))
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- #6 cors ------------------------------------------------------------------------

// corsScope reports the CORS-scoped paths: /api*, /api-browser*, and
// /{o}/{r}/api[-browser]/… (§2.3). No CORS headers anywhere else.
func corsScope(path string) bool {
	if strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/api-browser") {
		return true
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) >= 3 {
		return segs[2] == "api" || segs[2] == "api-browser"
	}
	return false
}

// originAllowed matches exact origins or one leading "*." label; the wildcard
// label must differ from the request's label — *.example.com does not match
// example.com (§2.3).
func originAllowed(allowed []string, origin string) bool {
	for _, a := range allowed {
		if a == origin {
			return true
		}
		if strings.HasPrefix(a, "*.") {
			label := a[2:] // "example.com"
			if strings.HasSuffix(origin, "."+label) {
				// The request's host label must differ from the wildcard label.
				host := origin
				if i := strings.Index(origin, "://"); i >= 0 {
					host = origin[i+3:]
				}
				if j := strings.IndexAny(host, "/:?"); j >= 0 {
					host = host[:j]
				}
				if !strings.HasSuffix(host, "."+label) {
					continue
				}
				return true
			}
		}
	}
	return false
}

const (
	corsAllowMethods = "GET, HEAD, POST, PUT, DELETE, OPTIONS"
	corsAllowHeaders = "Authorization, Content-Type, Accept, If-None-Match, X-Requested-With"
	corsExpose       = "ETag, Cache-Control, Content-Type, Location"
)

// cors implements §2.3 by hand (chi/cors NOT used). Preflight → 204
// unauthenticated; foreign non-same-origin state-changing request → 403
// before any handler runs; credentials mode is always include so the
// allow-origin value is never `*`.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !corsScope(r.URL.Path) || origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		allowed := originAllowed(s.cfg.Server.CorsOrigins, origin) || s.isCanonicalOrigin(origin) ||
			sameOriginHost(origin, r.Host)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", corsAllowMethods)
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", "600")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !allowed && r.Method != http.MethodGet && r.Method != http.MethodHead {
			plainStatus(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if allowed {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", corsExpose)
		}
		next.ServeHTTP(w, r)
	})
}

// isCanonicalOrigin reports the configured public_url origin itself.
func (s *Server) isCanonicalOrigin(origin string) bool {
	pub := s.cfg.Server.PublicURL
	return pub != "" && (origin == pub || strings.TrimSuffix(origin, "/") == strings.TrimSuffix(pub, "/"))
}

// sameOriginHost reports whether the Origin header refers to the request's
// own host:port. Browsers attach Origin to EVERY state-changing request,
// same-origin ones included — refusing those would break every UI write
// (first-run setup save included). Scheme is not compared: it is not
// recoverable from r.Host and a same-host scheme flip is a deployment
// choice (TLS termination), not an attacker's tool.
func sameOriginHost(origin, host string) bool {
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	o := origin[i+3:]
	if j := strings.IndexAny(o, "/?"); j >= 0 {
		o = o[:j]
	}
	return strings.EqualFold(o, host)
}

// --- #7 refreshSession ---------------------------------------------------------------

// refreshSession slides the session cookie: a valid walgit_session older than
// session_ttl/4 AND a principal that still passes → re-issue via Set-Cookie
// on app responses (skip /_auth/*). Stateless: age comes from the token's iat
// (§8.5). Runs after the handler so it can see the final principal.
func (s *Server) refreshSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/_auth/") {
			return
		}
		c, err := r.Cookie("walgit_session")
		if err != nil || c.Value == "" {
			return
		}
		tok, verr := s.authSvc.VerifyToken(c.Value)
		if verr != nil || tok.Kind != "session" {
			return
		}
		ttl := time.Duration(s.cfg.Server.Auth.SessionTTL)
		if ttl <= 0 || s.Now().Sub(tok.IssuedAt) <= ttl/4 {
			return
		}
		if fresh, merr := s.authSvc.MintSession(tok.Email); merr == nil {
			s.setSessionCookie(w, fresh)
		}
	})
}

// setSessionCookie applies the §8.5 cookie attributes.
func (s *Server) setSessionCookie(w http.ResponseWriter, tok SessionToken) {
	http.SetCookie(w, &http.Cookie{
		Name:     "walgit_session",
		Value:    tok.Wire,
		Path:     "/",
		MaxAge:   int(time.Duration(s.cfg.Server.Auth.SessionTTL).Seconds()),
		HttpOnly: true,
		Secure:   s.tlsOn || len(s.cfg.Server.CorsOrigins) > 0,
		SameSite: sameSiteFor(s.cfg.Server.CorsOrigins),
	})
}

func sameSiteFor(origins []string) http.SameSite {
	if len(origins) > 0 {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

// maybeRefreshSession re-issues the session cookie when sliding applies.
func (s *Server) maybeRefreshSession(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if strings.HasPrefix(r.URL.Path, "/_auth/") || p.Anonymous {
		return
	}
	c, err := r.Cookie("walgit_session")
	if err != nil || c.Value == "" {
		return
	}
	tok, verr := s.authSvc.VerifyToken(c.Value)
	if verr != nil || tok.Kind != "session" {
		return
	}
	ttl := time.Duration(s.cfg.Server.Auth.SessionTTL)
	if ttl <= 0 || s.Now().Sub(tok.IssuedAt) <= ttl/4 {
		return
	}
	if fresh, merr := s.authSvc.MintSession(tok.Email); merr == nil {
		s.setSessionCookie(w, fresh)
	}
}

// --- #8 requireAuth (gated group only) -------------------------------------------------

// requireAuth is NOT in the main chain: attached with Use inside the gated
// route group (SPA shell, /_ui/*, /services/setup.json, /metrics, and the
// owner/repo UI pages reached through the wildcard; §3.1). On failure: a
// browser-ish GET without Authorization and browser login enabled → 307
// /_auth/login?next=…; else the mapped status with a plain-text body naming
// the setup command.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, aerr := s.authSvc.Authenticate(r, s.cfg)
		switch {
		case aerr != nil:
			s.authFailure(w, r, aerr)
		case p.Anonymous && !s.cfg.Server.Auth.AnonymousRead:
			s.authFailure(w, r, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		default:
			s.maybeRefreshSession(w, r, p)
			next.ServeHTTP(w, injectPrincipal(r, p))
		}
	})
}

// injectPrincipal attaches the identity for the api seam (api.WithPrincipal)
// and server handlers.
func injectPrincipal(r *http.Request, p auth.Principal) *http.Request {
	ctx := context.WithValue(r.Context(), ctxPrincipalKey{}, p)
	return r.WithContext(api.WithPrincipal(ctx, p))
}

// authFailure maps the gated-group outcomes.
func (s *Server) authFailure(w http.ResponseWriter, r *http.Request, aerr *auth.AuthError) {
	switch aerr.Kind {
	case auth.ErrForbidden:
		plainStatus(w, http.StatusForbidden, aerr.Why+" — see: walhub setup tokens")
	case auth.ErrUnavailable:
		w.Header().Set("Retry-After", "15")
		plainStatus(w, http.StatusServiceUnavailable, aerr.Why)
	default: // ErrInvalid / ErrUnauthorized
		if r.Method == http.MethodGet && browserLooks(r) &&
			r.Header.Get("Authorization") == "" && s.authSvc.BrowserLoginEnabled() {
			http.Redirect(w, r, "/_auth/login?next="+r.URL.RequestURI(),
				http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		plainStatus(w, http.StatusUnauthorized, aerr.Why)
	}
}

// mapAuthStatus is the shared 401/403/503 mapping (§8.6) used inside handlers.
func (s *Server) mapAuthStatus(w http.ResponseWriter, aerr *auth.AuthError) {
	switch aerr.Kind {
	case auth.ErrForbidden:
		plainStatus(w, http.StatusForbidden, aerr.Why)
	case auth.ErrUnavailable:
		w.Header().Set("Retry-After", "15")
		plainStatus(w, http.StatusServiceUnavailable, aerr.Why)
	default:
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		plainStatus(w, http.StatusUnauthorized, aerr.Why)
	}
}

// --- #9 compress -------------------------------------------------------------------------

// compress is attached ONLY to the three web route groups (JSON API lanes,
// repo JSON lanes, UI). gzip at the fastest level; NEVER on git smart HTTP,
// bundles, LFS bytes, or SSE streams (streamed answers are never compressed);
// precompressed assets pass through untouched.
// Deviation: brotli is not in the stdlib and the dependency budget is closed
// (chi/toml/x.net only) — gzip-only.
func (s *Server) compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{w: &recorder{ResponseWriter: w}}
		next.ServeHTTP(cw, r)
		cw.close()
	})
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip")
}

// compressWriter lazily opens the gzip stream on first Write; SSE responses
// and precompressed bodies pass through untouched.
type compressWriter struct {
	w      http.ResponseWriter
	gz     *gzip.Writer
	bypass bool
}

func (cw *compressWriter) Header() http.Header { return cw.w.Header() }

func (cw *compressWriter) WriteHeader(status int) {
	ct := cw.w.Header().Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") || cw.w.Header().Get("Content-Encoding") != "" {
		cw.bypass = true // SSE; or the asset arrived precompressed
	} else {
		h := cw.w.Header()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		h.Del("Content-Length")
	}
	cw.w.WriteHeader(status)
}

func (cw *compressWriter) Write(b []byte) (int, error) {
	if cw.bypass {
		return cw.w.Write(b)
	}
	if cw.gz == nil {
		if cw.w.Header().Get("Content-Encoding") != "gzip" {
			cw.w.Header().Set("Content-Encoding", "gzip")
			cw.w.Header().Add("Vary", "Accept-Encoding")
			cw.w.Header().Del("Content-Length")
		}
		cw.gz = gzip.NewWriter(cw.w)
	}
	return cw.gz.Write(b)
}

func (cw *compressWriter) Flush() {
	if cw.gz != nil {
		_ = cw.gz.Flush()
	}
	if f, ok := cw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *compressWriter) close() {
	if cw.gz != nil {
		_ = cw.gz.Close()
	}
}

// --- helpers ------------------------------------------------------------------------------

// truncate shortens s to n bytes.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
