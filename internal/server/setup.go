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

// setupUI answers GET /setup with the SPA shell (D-WEB-6: /setup is a SPA
// route). The SHELL is gated by the §3.4 access rules; the app then renders
// the access-restricted card when the API refuses. The API itself keeps its
// own setupAccess gating (setup_api.go).
func (s *Server) setupUI(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	s.serveSPA(w, r)
}
