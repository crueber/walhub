package server

import (
	"net/http"
	"os"
	"strings"
)

// setupAccess implements the §3.4 access rules: open while (no config file)
// OR (config invalid) OR (auth mode = none); otherwise an admin principal is
// required. Optional WALHUB_SETUP_TOKEN: when set, the routes require it
// (query ?token= or Bearer) and skip the admin check.
func (s *Server) setupAccess(w http.ResponseWriter, r *http.Request) bool {
	if tok := os.Getenv("WALHUB_SETUP_TOKEN"); tok != "" {
		q := r.URL.Query().Get("token")
		h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtleMatch(tok, q) || subtleMatch(tok, h) {
			return true
		}
		plainStatus(w, http.StatusUnauthorized, "setup token required")
		return false
	}
	open := s.boot.Mode == "defaults" || s.boot.InSetupOnly() ||
		s.cfg.Server.Auth.Mode == "none"
	if open {
		return true
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.mapAuthStatus(w, aerr)
		return false
	}
	if aerr := requireAdmin(p); aerr != nil {
		s.mapAuthStatus(w, aerr)
		return false
	}
	return true
}

func subtleMatch(want, got string) bool {
	if got == "" || len(want) != len(got) {
		return false
	}
	v := 0
	for i := range want {
		v |= int(want[i] ^ got[i])
	}
	return v == 0
}

// setupUI serves the setup page (plain ESM page, D2): groups ALL config keys
// by section with current effective values, validates, saves via the API.
func (s *Server) setupUI(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	banner := ""
	if s.cfg.Server.Auth.Mode == "none" {
		banner = `<div class="banner">auth mode is "none" — anyone on the network has write+admin.
Pick a real auth mode below (recommended fix).</div>`
	}
	body := strings.Replace(setupPageHTML, `<div class="banner-slot"></div>`, banner, 1)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// setupUIAssets serves the setup page's ESM/CSS from the embedded FS
// (precompressed, like /_ui).
func (s *Server) setupUIAssets(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/setup/assets/")
	name = strings.TrimSuffix(name, ".gz")
	b, ok := setupAsset(name)
	if !ok {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// setupPageHTML is the standalone shell for the real setup page module
// (web/src/pages/setup.js): the import map resolves its bare `lib/…`
// specifiers to /setup/assets/lib/…, served by setupUIAssets.
const setupPageHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>walhub setup</title>
  <script type="importmap">{"imports":{"lib/":"/setup/assets/lib/"}}</script>
  <link rel="stylesheet" href="/setup/assets/setup.css">
</head>
<body>
  <h1>walhub setup</h1>
  <div class="banner-slot"></div>
  <main id="app"></main>
  <script type="module">
    import { mount } from "/setup/assets/setup.js";
    mount(document.getElementById("app"));
  </script>
</body></html>`
