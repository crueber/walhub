package server

import (
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"text/template"
)

// The install.sh + credential helper are served from embed templates rather
// than shipped as separate files — single-binary packaging and one source of
// truth for the host-slug rules (§9.2/§9.3).

//go:embed templates/install.sh.tmpl
var installShTmpl string

//go:embed templates/credential-helper.sh.tmpl
var credentialHelperTmpl string

type installTmplData struct {
	Host         string
	Base         string
	Slug         string
	AuthNone     bool
	Repo         string
	TokenURL     string
	CASelfSigned bool
	Helper       string // the rendered per-host credential helper (§9.3)
}

// renderInstall renders the script with the host baked in (§9.2). The
// credential helper is rendered from its own template and inlined.
func renderInstall(d installTmplData) (string, error) {
	helper, err := credentialHelper(d)
	if err != nil {
		return "", err
	}
	d.Helper = helper
	t, err := template.New("install").Parse(installShTmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// installSh answers GET /services/public/install.sh[?repo=|tree=] (§9.2):
// text/x-shellscript, Cache-Control: public, max-age=300.
func (s *Server) installSh(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	host := r.Host
	d := installTmplData{
		Host:         host,
		Base:         base,
		Slug:         hostSlug(host),
		AuthNone:     s.cfg.Server.Auth.Mode == "none",
		Repo:         r.URL.Query().Get("repo"),
		TokenURL:     base + "/_auth/tokens",
		CASelfSigned: s.tlsOn || s.cfg.Server.TLS.Mode == "self_signed",
	}
	body, err := renderInstall(d)
	if err != nil {
		plainStatus(w, http.StatusInternalServerError, "template render failed")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// credentialHelper renders the per-host credential helper (§9.3; the single
// source of truth for the get/store/erase protocol).
func credentialHelper(d installTmplData) (string, error) {
	t, err := template.New("helper").Parse(credentialHelperTmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

var _ = fmt.Sprintf
