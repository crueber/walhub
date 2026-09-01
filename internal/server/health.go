package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	web "git.packden.us/crueber/walhub/web"
)

// writeJSONBody emits a JSON body (server-side; the api seam has its own).
func writeJSONBody(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		plainStatus(w, http.StatusInternalServerError, "encode: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// healthz answers 200 {"status":"ok","version":…} — even in setup-only mode,
// so orchestrators can tell "up but unconfigured" from "down" (§10.1).
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSONBody(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.Version(),
	})
}

// prewarmPending reports the outstanding prewarm count (composition injects
// the value; the server only reports it).
var prewarmPending int64

// SetPrewarmPending is called by the prewarm loop.
func SetPrewarmPending(n int64) { prewarmPending = n }

// readyz implements §10.2: 200 ready (+ prewarm_pending, instance, placement,
// warnings, config:"defaults" in defaults-banner mode) or 503 warming /
// draining / setup_required.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.boot.InSetupOnly() {
		writeJSONBody(w, http.StatusServiceUnavailable, map[string]any{
			"status": "setup_required",
			"errors": s.boot.Errors,
		})
		return
	}
	if s.drain.Phase() == 2 {
		w.Header().Set("Retry-After", "15")
		writeJSONBody(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "draining",
			"running": s.inflight.N(),
		})
		return
	}
	if p := prewarmPending; p > 0 && !s.prewarmTimedOut() {
		writeJSONBody(w, http.StatusServiceUnavailable, map[string]any{
			"status":          "warming",
			"running":         s.inflight.N(),
			"prewarm_pending": p,
		})
		return
	}
	body := map[string]any{
		"status":          "ready",
		"prewarm_pending": prewarmPending,
		"instance":        s.instance,
		"placement": map[string]bool{
			"serve":            true,
			"serve_exclude":    false,
			"maintain":         true,
			"maintain_exclude": false,
		},
	}
	if s.cfg.Server.Auth.Mode == "none" {
		body["warnings"] = []string{"auth_none"}
	}
	if s.boot.Mode == "defaults" {
		body["config"] = "defaults"
	}
	writeJSONBody(w, http.StatusOK, body)
}

var prewarmStart = time.Now()

// prewarmTimedOut: cache.prewarm_ready_timeout elapsed (0 = don't gate) —
// readiness no longer blocks on prewarm (§10.2).
func (s *Server) prewarmTimedOut() bool {
	t := time.Duration(s.cfg.Cache.PrewarmReadyTimeout)
	if t <= 0 {
		return false
	}
	return time.Since(prewarmStart) > t
}

// sdkReposJS serves the esbuild bundle of the SDK submodules
// (web/dist/repos.js, built by `make web`, embedded raw — D-WEB-2/D-WEB-3):
// text/javascript, no-cache + strong ETag + 304.
func (s *Server) sdkReposJS(w http.ResponseWriter, r *http.Request) {
	b, ok := webAsset("dist/repos.js")
	if !ok {
		plainStatus(w, http.StatusNotFound, "repos.js is not built — run make web")
		return
	}
	h := sdkETag(string(b))
	etag := `"sdk-` + h + `"`
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchHeader(inm, "sdk-"+h) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// caPem publishes the self-signed cert — only when this host terminates TLS
// itself (§3.3); else 404.
func (s *Server) caPem(w http.ResponseWriter, r *http.Request) {
	if !s.tlsOn && s.cfg.Server.TLS.Mode == "" {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	b, err := s.loadCACert()
	if err != nil {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// eventsNotify answers POST /_events/notify — handler-authenticated
// (09_events.md); the events bridge is not wired in this package.
func (s *Server) eventsNotify(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	if aerr := requireWrite(p); aerr != nil {
		// A bare return would emit an empty 200; map it like every other
		// handler-authenticated route (§8.6).
		s.mapAuthStatus(w, aerr)
		return
	}
	if s.notify != nil {
		s.notify(r.URL.Query().Get("repo"))
	}
	plainStatus(w, http.StatusAccepted, "accepted")
}

// spaHome answers GET / (SPA shell; ?format=text or text Accept → plain
// one-per-line repo list) — gated (§3.3).
func (s *Server) spaHome(w http.ResponseWriter, r *http.Request) {
	owners, err := s.api.Owners(r)
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	accept := r.Header.Get("Accept")
	if r.URL.Query().Get("format") == "text" ||
		strings.Contains(accept, "text/plain") && !strings.Contains(accept, "text/html") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		for _, o := range owners {
			_, _ = w.Write([]byte(o + "\n"))
		}
		return
	}
	s.serveSPA(w, r)
}

// ownerPage answers /{owner} (gated; index.html, no-cache).
func (s *Server) ownerPage(w http.ResponseWriter, r *http.Request) {
	s.serveSPA(w, r)
}

// repoPage answers /{owner}/{repo} and the UI page routes
// (tree/blob/commits/commit/wal/settings — index.html, no-cache; §3.3).
func (s *Server) repoPage(id interface{ String() string }) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.serveSPA(w, r)
	}
}

// serveSPA answers every UI route with the embedded SPA shell (web/index.html:
// import map + raw ES modules — no-cache + ETag, §3.3/D-WEB-3).
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	b, ok := webAsset("index.html")
	if !ok {
		plainStatus(w, http.StatusInternalServerError, "ui shell missing — run make build")
		return
	}
	h := sdkETag(string(b))
	etag := `"` + h + `"`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchHeader(inm, h) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// setupJSON answers GET /services/setup.json[?repo=] — read-authed, no-cache
// (§9.1): exact field names, every UI surface renders recipes from here.
func (s *Server) setupJSON(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	if aerr := requireRead(p, s.cfg.Server.Auth.AnonymousRead); aerr != nil {
		s.mapAuthStatus(w, aerr)
		return
	}
	base := s.baseURL(r)
	repo := r.URL.Query().Get("repo")
	authNone := s.cfg.Server.Auth.Mode == "none"
	extraHeader := ""
	if !authNone {
		extraHeader = ` -c http.extraHeader="Authorization: Bearer $WALGIT_TOKEN"`
	}
	manual := fmt.Sprintf("git%s -c transfer.bundleURI=true -c fetch.bundleURI=%s/%s.git/bundles/catchup clone %s/%s.git",
		extraHeader, base, repo, base, repo)
	plain := fmt.Sprintf("git clone -c fetch.bundleURI=%s/%s.git/bundles/catchup %s/%s.git",
		base, repo, base, repo)
	blobless := fmt.Sprintf(
		"git clone --filter=blob:none --sparse --bundle-uri=%s/%s.git/bundles/list?filter=blob:none -c fetch.bundleURI=%s/%s.git/bundles/catchup?filter=blob:none %s/%s.git",
		base, repo, base, repo, base, repo)
	body := map[string]any{
		"base_url":       base,
		"host":           r.Host,
		"install":        "curl -fsSL " + base + "/services/public/install.sh | sh -s -- " + repo,
		"install_url":    base + "/services/public/install.sh?repo=" + repo,
		"manual_clone":   manual,
		"plain_clone":    plain,
		"blobless_clone": blobless,
		"bundle_list":    base + "/" + repo + ".git/bundles/list",
		"setup_text":     "export WALGIT_TOKEN=$(cat ~/.config/git/" + hostSlug(r.Host) + "-token)",
	}
	if s.cfg.Server.Auth.Mode == "oidc" {
		body["token_url"] = base + "/_auth/tokens"
	}
	if s.tlsOn || s.cfg.Server.TLS.Mode == "self_signed" {
		body["ca_url"] = base + "/services/public/ca.pem"
		body["trust"] = "git config --global http." + base + "/.sslCAInfo ~/.config/git/" +
			hostSlug(r.Host) + "-ca.pem"
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSONBody(w, http.StatusOK, body)
}

// hostSlug is the §9.2 slug rule: every non-[a-z0-9] char → "-" (host-derived
// so two walgit hosts coexist).
func hostSlug(host string) string {
	var b []byte
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	return string(b)
}

// serveUIAssets answers /_ui/* assets (gated; precompressed, pass through
// untouched) (§3.3). The SPA asset FS is embedded at packaging time.
func (s *Server) serveUIAssets(w http.ResponseWriter, r *http.Request) {
	name := stringsTrimPrefix(r.URL.Path, "/_ui/")
	if b, ok := uiAsset(name); ok {
		switch {
		case hasSuffixFold(name, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case hasSuffixFold(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case hasSuffixFold(name, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		h := sdkETag(string(b))
		w.Header().Set("ETag", `"`+h+`"`)
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchHeader(inm, h) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(b)
		}
		return
	}
	plainStatus(w, http.StatusNotFound, "not found")
}

func stringsTrimPrefix(s, p string) string {
	if hasPrefixFold(s, p) {
		return s[len(p):]
	}
	return s
}

func hasSuffixFold(s, suf string) bool {
	return len(s) >= len(suf) && equalFoldLast(s, suf)
}

func equalFoldLast(s, suf string) bool {
	for i := 0; i < len(suf); i++ {
		a, b := s[len(s)-len(suf)+i], suf[i]
		if a == b {
			continue
		}
		if 'A' <= a && a <= 'Z' && a+('a'-'A') == b {
			continue
		}
		if 'a' <= a && a <= 'z' && a-('a'-'A') == b {
			continue
		}
		return false
	}
	return true
}

// webAsset reads one path from the embedded UI tree (web/embed.go: index.html,
// dist/, sdk/, src/, css/). Names are /-separated embed paths; traversal-safe
// because embed.FS paths cannot escape the tree.
func webAsset(name string) ([]byte, bool) {
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return nil, false
	}
	b, err := web.Files.ReadFile(name)
	if err != nil {
		return nil, false
	}
	return b, true
}

// uiAsset maps /_ui/<name> to the embedded UI tree (12_web_ui.md §1.0: the
// import map points at /_ui/sdk/src/index.js, /_ui/src/…, /_ui/css/…).
func uiAsset(name string) ([]byte, bool) {
	switch {
	case name == "index.html" ||
		strings.HasPrefix(name, "dist/") ||
		strings.HasPrefix(name, "sdk/") ||
		strings.HasPrefix(name, "src/") ||
		strings.HasPrefix(name, "css/"):
		return webAsset(name)
	}
	return nil, false
}

// setupAsset maps /setup/assets/<name> to the standalone setup page's files:
// the page entry (src/pages/setup.js), its CSS, and the lib/ modules its bare
// specifiers import (resolved by the page's import map → /setup/assets/lib/…).
func setupAsset(name string) ([]byte, bool) {
	switch {
	case name == "setup.js":
		return webAsset("src/pages/setup.js")
	case name == "setup.css":
		return webAsset("css/setup.css")
	case strings.HasPrefix(name, "lib/"):
		return webAsset("src/" + name)
	}
	return nil, false
}
