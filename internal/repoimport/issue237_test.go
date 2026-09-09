// issue237_test.go — Forgejo crueber/walhub#237 regressions (SSRF allowlist
// bypass + anonymous dangerous): every bypass row from the issue table is
// pinned, the clone URL is proven equal to the gated canonical string
// through the real Begin→drive→CloneMirror path, and dangerous:true from
// the request body requires an authenticated admin (or the operator CLI).
//
// No network, no real git binary here: DNS is stubbed at the CheckSSRF
// level, the git binary is a capture script, and allowlist tests run
// against "localhost" with allow-private so checkPrivate short-circuits
// before any resolution.
package repoimport

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// issue237Variants are the bypass rows from the issue table plus the
// canonical form first: all must collapse to issue237Canon.
var issue237Variants = []string{
	"https://git.packden.us/crueber/dotfiles.git",
	"https://git.packden.us:443/crueber/dotfiles.git",
	"https://git.packden.us/crueber/dotfiles/",
	"https://git.packden.us/crueber/dotfiles.git/",
	"https://GIT.PACKDEN.US/crueber/dotfiles.git",
}

const issue237Canon = "https://git.packden.us/crueber/dotfiles"

// TestNormalizeCanonical237 pins the normalization: every bypass variant
// produces the SAME Normalized.Host and canonical URL as the plain form —
// one canonical source, one gate decision, one import.json provenance
// match.
func TestNormalizeCanonical237(t *testing.T) {
	for _, in := range issue237Variants {
		n, err := NormalizeSource(in)
		if err != nil {
			t.Fatalf("NormalizeSource(%q) error: %v", in, err)
		}
		if n.URL != issue237Canon {
			t.Fatalf("NormalizeSource(%q).URL = %q, want %q", in, n.URL, issue237Canon)
		}
		if n.Host != "git.packden.us" {
			t.Fatalf("NormalizeSource(%q).Host = %q, want %q", in, n.Host, "git.packden.us")
		}
		if n.Scheme != "https" {
			t.Fatalf("NormalizeSource(%q).Scheme = %q, want https", in, n.Scheme)
		}
		if n.Kind != SourceGeneric {
			t.Fatalf("NormalizeSource(%q).Kind = %q, want generic", in, n.Kind)
		}
	}
	// The http twin is a distinct source (scheme preserved) but canonical
	// within its scheme — documented §1 scope (http without tokens), never
	// silently upgraded or folded into the https form.
	n, err := NormalizeSource("http://git.packden.us/crueber/dotfiles.git")
	if err != nil {
		t.Fatalf("http twin error: %v", err)
	}
	if n.URL != "http://git.packden.us/crueber/dotfiles" || n.Scheme != "http" {
		t.Fatalf("http twin = %+v, want canonical http form", n)
	}
	n80, err := NormalizeSource("http://git.packden.us:80/crueber/dotfiles.git")
	if err != nil {
		t.Fatalf("http :80 error: %v", err)
	}
	if n80.URL != n.URL {
		t.Fatalf(":80 URL = %q, want %q (default-port strip)", n80.URL, n.URL)
	}
	// GitHub default-port folds into the existing canonical form.
	gh, err := NormalizeSource("https://github.com:443/acme/monorepo.git")
	if err != nil {
		t.Fatalf("github :443 error: %v", err)
	}
	if gh.URL != "https://github.com/acme/monorepo.git" {
		t.Fatalf("github :443 URL = %q, want canonical", gh.URL)
	}
	// IPv6 literals keep their brackets in the canonical form.
	v6, err := NormalizeSource("https://[::1]:443/x.git")
	if err != nil {
		t.Fatalf("ipv6 error: %v", err)
	}
	if v6.URL != "https://[::1]/x" || v6.Host != "::1" {
		t.Fatalf("ipv6 = %+v, want bracketed canonical", v6)
	}
}

// TestNormalizeRejectsPorts237 pins the fail-closed port rule: a
// non-default explicit port is refused (allowlist entries are plain
// hosts, so a ported URL can never legitimately match).
func TestNormalizeRejectsPorts237(t *testing.T) {
	for _, in := range []string{
		"https://git.packden.us:8443/crueber/dotfiles.git",
		"https://git.packden.us:80/crueber/dotfiles.git", // http default on an https URL
		"https://git.packden.us:9418/crueber/dotfiles.git",
		"http://git.packden.us:8080/crueber/dotfiles.git",
		"https://github.com:8080/acme/monorepo.git",
	} {
		n, err := NormalizeSource(in)
		if err == nil {
			t.Fatalf("NormalizeSource(%q) = %+v, want 400 explicit-port refusal", in, n)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Status != 400 || !strings.Contains(se.Message, "explicit port") {
			t.Fatalf("NormalizeSource(%q) err = %v, want 400 naming the port rule", in, err)
		}
	}
}

// TestIsDefaultPort237 pins the port table directly (every scheme branch
// plus the fail-closed fallthrough).
func TestIsDefaultPort237(t *testing.T) {
	for _, tc := range []struct {
		scheme, port string
		want         bool
	}{
		{"https", "443", true}, {"https", "80", false}, {"https", "8443", false},
		{"http", "80", true}, {"http", "443", false}, {"http", "8080", false},
		{"ssh", "22", true}, {"ssh", "2222", false},
		{"git", "9418", true}, {"git", "22", false},
		{"file", "x", false}, {"bogus", "1", false},
	} {
		if got := isDefaultPort(tc.scheme, tc.port); got != tc.want {
			t.Fatalf("isDefaultPort(%q, %q) = %v, want %v", tc.scheme, tc.port, got, tc.want)
		}
	}
	// Cross-scheme normalization: default ports fold, others refuse.
	for _, tc := range []struct {
		in   string
		url  string
		fail bool
	}{
		{in: "ssh://example.com:22/team/proj.git", url: "ssh://example.com/team/proj"},
		{in: "ssh://example.com:2222/team/proj.git", fail: true},
		{in: "git://example.com:9418/proj.git", url: "git://example.com/proj"},
		{in: "git://example.com:22/proj.git", fail: true},
	} {
		n, err := NormalizeSource(tc.in)
		if tc.fail {
			if err == nil {
				t.Fatalf("NormalizeSource(%q) = %+v, want refusal", tc.in, n)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeSource(%q) error: %v", tc.in, err)
		}
		if n.URL != tc.url {
			t.Fatalf("NormalizeSource(%q).URL = %q, want %q", tc.in, n.URL, tc.url)
		}
	}
}

// TestCheckSSRFVariantParity237 pins gate parity: every variant resolves to
// the same host check (stub records the resolved host), passes an
// allowlist holding the canonical host (mixed-case entry proves
// case-insensitivity on both sides), and fails identically when the host
// is not allowlisted.
func TestCheckSSRFVariantParity237(t *testing.T) {
	var resolved []string
	stub := func(host string) ([]net.IP, error) {
		resolved = append(resolved, host)
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	allow := SSRFConfig{Allowlist: []string{"GIT.packden.us"}}
	for _, in := range issue237Variants {
		n, err := NormalizeSource(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckSSRF(n, allow, stub); err != nil {
			t.Fatalf("CheckSSRF(%q) = %v, want allowlisted pass", in, err)
		}
	}
	if len(resolved) != len(issue237Variants) {
		t.Fatalf("resolved %d hosts, want %d", len(resolved), len(issue237Variants))
	}
	for _, h := range resolved {
		if h != "git.packden.us" {
			t.Fatalf("resolved host = %q, want canonical %q", h, "git.packden.us")
		}
	}
	deny := SSRFConfig{Allowlist: []string{"other.example"}}
	var first string
	for _, in := range issue237Variants {
		n, err := NormalizeSource(in)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckSSRF(n, deny, stub)
		if err == nil {
			t.Fatalf("CheckSSRF(%q) passed a foreign allowlist", in)
		}
		if first == "" {
			first = err.Error()
			if !strings.Contains(first, "not in import.url_allowlist") {
				t.Fatalf("err = %q, want allowlist refusal", first)
			}
		} else if err.Error() != first {
			t.Fatalf("CheckSSRF(%q) err = %q, want identical %q", in, err.Error(), first)
		}
	}
}

// TestDangerousAllowed237 pins the authority rule: only an authenticated
// admin under a real auth mode (token/oidc) may wield dangerous:true over
// HTTP. The auth-none principal carries Admin but is unauthenticated —
// never qualifies. Unknown modes fail closed.
func TestDangerousAllowed237(t *testing.T) {
	admin := auth.Principal{Name: "root", Write: true, Admin: true}
	writer := auth.Principal{Name: "writer", Write: true}
	for _, tc := range []struct {
		name string
		mode string
		p    auth.Principal
		want bool
	}{
		{"none admin rejected", "none", auth.None(), false},
		{"none token-admin rejected", "none", admin, false},
		{"empty-mode admin rejected", "", admin, false},
		{"token admin allowed", "token", admin, true},
		{"oidc admin allowed", "oidc", admin, true},
		{"token writer rejected", "token", writer, false},
		{"oidc writer rejected", "oidc", writer, false},
		{"token anonymous rejected", "token", auth.Anonymous(), false},
		{"bogus mode admin rejected", "bogus", admin, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := testService(t, nil, &FakeRoles{})
			svc.cfg.Server.Auth.Mode = tc.mode
			if got := svc.dangerousAllowed(tc.p); got != tc.want {
				t.Fatalf("dangerousAllowed(mode=%q, %+v) = %v, want %v", tc.mode, tc.p, got, tc.want)
			}
		})
	}
	// Nil cfg fails closed (never a nil-map-style panic, never allowed).
	if (&Service{}).dangerousAllowed(admin) {
		t.Fatalf("nil-cfg dangerousAllowed(admin) = true, want false")
	}
}

// TestBeginDangerousGate237 pins the handler path: anonymous dangerous is
// rejected (401 via the auth gate), authenticated non-admin and auth-none
// dangerous are rejected (403 naming the CLI), while an authenticated
// admin's dangerous import of a real fixture proceeds (legitimate flow
// keeps working).
func TestBeginDangerousGate237(t *testing.T) {
	body := func(src string, dangerous bool) string {
		return fmt.Sprintf(`{"source_url":%q,"owner":"acme","name":"w","dangerous":%v}`, src, dangerous)
	}
	t.Run("anonymous dangerous 401", func(t *testing.T) {
		svc, _ := testService(t, nil, &FakeRoles{})
		h := testHandler(svc, auth.Anonymous())
		w := doPost(t, h, "/api/v1/repos/imports", body("file:///srv/r.git", true), "")
		if w.Code != 401 {
			t.Fatalf("status = %d, want 401 (body %q)", w.Code, w.Body.String())
		}
	})
	t.Run("writer dangerous 403", func(t *testing.T) {
		svc, _ := testService(t, nil, &FakeRoles{})
		h := testHandler(svc, writerPrincipal())
		w := doPost(t, h, "/api/v1/repos/imports", body("file:///srv/r.git", true), "")
		if w.Code != 403 {
			t.Fatalf("status = %d, want 403 (body %q)", w.Code, w.Body.String())
		}
	})
	t.Run("auth-none dangerous 403 names CLI", func(t *testing.T) {
		svc, _ := testService(t, nil, &FakeRoles{})
		h := testHandler(svc, auth.None())
		w := doPost(t, h, "/api/v1/repos/imports", body("file:///srv/r.git", true), "")
		if w.Code != 403 {
			t.Fatalf("status = %d, want 403 (body %q)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "walhub import --dangerous") {
			t.Fatalf("body = %q, want the CLI pointer", w.Body.String())
		}
	})
	t.Run("token-mode admin dangerous imports a real fixture", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Server.Auth.Mode = "token"
		svc, _ := testService(t, cfg, &FakeRoles{})
		h := testHandler(svc, auth.Principal{Name: "root", Write: true, Admin: true})
		src := fixtureRepo(t, t.TempDir()+"/src", 1, 0, 0)
		w := doPost(t, h, "/api/v1/repos/imports", body(src, true), "")
		if w.Code != 202 {
			t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
		}
		var out struct {
			Task map[string]any `json:"task"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		id, _ := out.Task["id"].(string)
		if id == "" {
			t.Fatalf("202 body lacks task id: %q", w.Body.String())
		}
		o := awaitDone(t, svc, id, 60_000_000_000)
		if o.Err != nil {
			t.Fatalf("fixture import failed: %+v", o.Err)
		}
		if o.SourceURL != src {
			t.Fatalf("provenance source_url = %q, want canonical %q", o.SourceURL, src)
		}
	})
}

// captureGit237 installs a fake git binary that appends its argv (one
// invocation per line, "$*") to outPath and exits 0. CloneMirror's argv
// carries the clone URL second-to-last, so the recorded lines prove
// exactly which string git was handed.
func captureGit237(t *testing.T, outPath string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gitcap.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + outPath + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func mirrorSrcURLs237(t *testing.T, outPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(line, "--mirror") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			t.Fatalf("unparseable capture line %q", line)
		}
		urls = append(urls, f[len(f)-2]) // argv: ... -- <srcURL> <dir>
	}
	return urls
}

// TestCloneReceivesCanonicalURL237 proves no check-vs-fetch divergence
// through the real Begin→drive→CloneMirror path: every issue-table variant
// POSTed against an allowlisted host starts (202), and the argv handed to
// git carries the single canonical URL — identical for all variants. The
// allowlist names localhost with allow-private so no DNS or network is
// touched (checkPrivate short-circuits on allow).
func TestCloneReceivesCanonicalURL237(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.URLAllowlist = []string{"localhost"}
	cfg.Import.AllowPrivateNetworks = true
	svc, _ := testService(t, cfg, &FakeRoles{})
	outPath := filepath.Join(t.TempDir(), "argv.log")
	if err := os.WriteFile(outPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	svc.git.Binary = captureGit237(t, outPath)
	h := testHandler(svc, adminPrincipal())
	variants := []string{
		"https://localhost/x/y.git",
		"https://localhost:443/x/y.git",
		"https://localhost/x/y/",
		"https://localhost/x/y.git/",
		"https://LOCALHOST/x/y.git",
	}
	for i, v := range variants {
		w := doPost(t, h, "/api/v1/repos/imports",
			fmt.Sprintf(`{"source_url":%q,"owner":"acme","name":"r%d"}`, v, i), "")
		if w.Code != 202 {
			t.Fatalf("POST %q status = %d, want 202 (body %q)", v, w.Code, w.Body.String())
		}
		var out struct {
			Task map[string]any `json:"task"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		id, _ := out.Task["id"].(string)
		if id == "" {
			t.Fatalf("202 body lacks task id: %q", w.Body.String())
		}
		// Sequential: the next Begin must not interleave argv lines in
		// the capture file (concurrent shell appends garble), and the
		// clone semaphore serializes real clones anyway.
		awaitDone(t, svc, id, 60_000_000_000) // clone "succeeds", ingest 422s — outcome irrelevant
	}
	urls := mirrorSrcURLs237(t, outPath)
	if len(urls) != len(variants) {
		t.Fatalf("recorded %d clone URLs, want %d", len(urls), len(variants))
	}
	for _, u := range urls {
		if u != "https://localhost/x/y" {
			t.Fatalf("clone URL = %q, want single canonical %q", u, "https://localhost/x/y")
		}
	}
	// The http twin stays a distinct canonical source (scheme preserved).
	w := doPost(t, h, "/api/v1/repos/imports",
		`{"source_url":"http://localhost/x/y.git","owner":"acme","name":"rhttp"}`, "")
	if w.Code != 202 {
		t.Fatalf("http twin status = %d, want 202 (documented http-without-token scope)", w.Code)
	}
}

// TestVariantGateParityHTTP237 pins handler-level parity for rejected
// hosts: every variant against a foreign allowlist fails identically (no
// DNS touched — the host mismatch precedes resolution), and a non-default
// port fails closed at normalization.
func TestVariantGateParityHTTP237(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.URLAllowlist = []string{"other.example"}
	svc, _ := testService(t, cfg, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	variants := []string{
		"https://localhost/x/y.git",
		"https://localhost:443/x/y.git",
		"https://localhost/x/y/",
		"https://localhost/x/y.git/",
		"https://LOCALHOST/x/y.git",
	}
	var first string
	for _, v := range variants {
		w := doPost(t, h, "/api/v1/repos/imports",
			fmt.Sprintf(`{"source_url":%q,"owner":"acme","name":"w"}`, v), "")
		if w.Code != 400 {
			t.Fatalf("POST %q status = %d, want 400", v, w.Code)
		}
		if first == "" {
			first = w.Body.String()
			if !strings.Contains(first, "not in import.url_allowlist") {
				t.Fatalf("body = %q, want allowlist refusal", first)
			}
		} else if w.Body.String() != first {
			t.Fatalf("POST %q body = %q, want identical %q", v, w.Body.String(), first)
		}
	}
	w := doPost(t, h, "/api/v1/repos/imports",
		`{"source_url":"https://localhost:8080/x/y.git","owner":"acme","name":"w"}`, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "explicit port") {
		t.Fatalf("non-default port status = %d body = %q, want 400 port refusal", w.Code, w.Body.String())
	}
}

// TestPortRefusedDespiteAllowlistMatch237 pins fail-closed precedence at
// the handler level: a non-default port is refused even when the host IS
// allowlisted (the port rule runs inside NormalizeSource, before the gate
// ever sees the host), and embedded userinfo never reaches the gate even
// on an allowlisted host.
func TestPortRefusedDespiteAllowlistMatch237(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.URLAllowlist = []string{"localhost"}
	cfg.Import.AllowPrivateNetworks = true
	svc, _ := testService(t, cfg, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	for _, v := range []string{
		"https://localhost:8080/x/y.git",
		"https://localhost:8443/x/y.git",
		"http://localhost:8080/x/y.git",
	} {
		w := doPost(t, h, "/api/v1/repos/imports",
			fmt.Sprintf(`{"source_url":%q,"owner":"acme","name":"w"}`, v), "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "explicit port") {
			t.Fatalf("POST %q status = %d body = %q, want 400 explicit-port refusal despite allowlist match", v, w.Code, w.Body.String())
		}
	}
	w := doPost(t, h, "/api/v1/repos/imports",
		`{"source_url":"https://user:token@localhost/x/y.git","owner":"acme","name":"w"}`, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "credentials") {
		t.Fatalf("userinfo status = %d body = %q, want 400 credentials refusal", w.Code, w.Body.String())
	}
}

// TestLegitimateCanonicalAllowlisted237 guards the "do NOT break
// legitimate imports" constraint: the plain canonical form against a
// matching allowlist starts normally (202). Clone output is captured so
// no network is touched.
func TestLegitimateCanonicalAllowlisted237(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.URLAllowlist = []string{"localhost"}
	cfg.Import.AllowPrivateNetworks = true
	svc, _ := testService(t, cfg, &FakeRoles{})
	outPath := filepath.Join(t.TempDir(), "argv.log")
	if err := os.WriteFile(outPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	svc.git.Binary = captureGit237(t, outPath)
	h := testHandler(svc, adminPrincipal())
	w := doPost(t, h, "/api/v1/repos/imports",
		`{"source_url":"https://localhost/x/y.git","owner":"acme","name":"w"}`, "")
	if w.Code != 202 {
		t.Fatalf("canonical allowlisted status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
}
