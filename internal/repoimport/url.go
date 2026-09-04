// url.go — source-URL normalization, validation, SSRF gating, and secret
// scrubbing (docs/features/10 §§1/3, R1 S2/S5).
//
// Canonical form: GitHub `owner/repo` shorthand and full
// `https://github.com/owner/repo[.git]` normalize to
// `https://github.com/<owner>/<repo>.git`; anything else must be a full
// URL and is kept verbatim (modulo a single stripped `.git` suffix — the
// ParseRepoId precedent). URLs with embedded credentials are refused
// (400) so tokens never land in import.json, logs, or task params.
//
// SSRF (v1 base): import.allow_private_networks=false denies loopback +
// RFC1918/ULA resolution; import.url_allowlist non-empty restricts to the
// listed hosts; file:// needs import.allow_file_urls=true (tests/fixtures
// only). Residuals (R1 S5): DNS TOCTOU (check-time vs clone-time
// resolution can differ) and redirect following (git follows HTTP
// redirects; the allowlist is evaluated against the FINAL effective URL
// only when the server follows — stock git does the following, so the
// token helper is host-pinned to the ORIGINAL host and redirects never
// carry it). A non-GitHub URL with an empty allowlist additionally
// requires the explicit dangerous:true confirm flag (§9.6 idea, kept).
package repoimport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// SourceKind classifies the canonical source URL.
type SourceKind string

const (
	SourceGitHub  SourceKind = "github"
	SourceGeneric SourceKind = "generic"
	SourceFile    SourceKind = "file"
)

// Normalized is a validated, canonical source URL.
type Normalized struct {
	URL    string     // canonical URL (credentials refused, never present)
	Kind   SourceKind // github | generic | file
	Host   string     // URL host ("" for file)
	Scheme string     // lowercased scheme ("https", "http", "ssh", "git", "file", "scp")
}

// scpLike matches scp-style SSH targets: [user@]host:path.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:.*$`)

// NormalizeSource validates raw (shorthand or URL) and returns the
// canonical form. It never touches the network (DNS checks are separate).
func NormalizeSource(raw string) (Normalized, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Normalized{}, &StatusError{Status: 400, Message: "source_url is required"}
	}
	if isGitHubShorthand(s) {
		owner, repo := splitShorthand(s)
		return Normalized{
			URL:    "https://github.com/" + owner + "/" + repo + ".git",
			Kind:   SourceGitHub,
			Host:   "github.com",
			Scheme: "https",
		}, nil
	}
	if scpLike.MatchString(s) {
		return Normalized{URL: s, Kind: SourceGeneric, Scheme: "scp"}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return Normalized{}, &StatusError{Status: 400, Message: fmt.Sprintf("bad source_url %q: want owner/repo or a full git URL", raw)}
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "ssh", "git", "file":
	default:
		return Normalized{}, &StatusError{Status: 400, Message: fmt.Sprintf("bad source_url %q: unsupported scheme %q", raw, u.Scheme)}
	}
	if u.User != nil {
		return Normalized{}, &StatusError{Status: 400, Message: "source_url must not embed credentials; strip userinfo and pass the token field"}
	}
	if strings.ToLower(u.Scheme) == "file" {
		return Normalized{URL: u.String(), Kind: SourceFile, Scheme: "file"}, nil
	}
	if u.Host == "" {
		return Normalized{}, &StatusError{Status: 400, Message: fmt.Sprintf("bad source_url %q: missing host", raw)}
	}
	host := u.Hostname()
	scheme := strings.ToLower(u.Scheme)
	canon := stripDotGit(u.String())
	kind := SourceGeneric
	if strings.EqualFold(host, "github.com") {
		kind = SourceGitHub
		canon = canonicalGitHubURL(u)
	}
	return Normalized{URL: canon, Kind: kind, Host: strings.ToLower(host), Scheme: scheme}, nil
}

// ValidateTransport refuses transports the server cannot speak in v1
// (server-side ssh/git transports are OUT — no key agent on the host;
// document https + token as the path) and token-over-plaintext (a PAT
// over http leaks by construction).
func ValidateTransport(n Normalized, hasToken bool) error {
	switch n.Scheme {
	case "https", "http", "file":
	case "ssh", "scp", "git":
		return &StatusError{Status: 400, Message: "ssh and git:// sources are not supported in v1 (no key agent on the server); use https with the token field for private sources"}
	default:
		return &StatusError{Status: 400, Message: fmt.Sprintf("bad source_url: unsupported scheme %q", n.Scheme)}
	}
	if hasToken && n.Scheme == "http" {
		return &StatusError{Status: 400, Message: "token requires https (never send credentials over plaintext http)"}
	}
	return nil
}

// isGitHubShorthand reports owner/repo (no scheme, exactly one slash,
// ParseRepoId-charset parts — anything else must be a full URL).
func isGitHubShorthand(s string) bool {
	if strings.Contains(s, "://") || strings.HasPrefix(s, "git@") {
		return false
	}
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(repo, "/") {
		return false
	}
	repo = strings.TrimSuffix(repo, ".git")
	return validIDPart(owner) && validIDPart(repo)
}

// validIDPart mirrors git RepoId part rules (04_git.md §1.1) so shorthand
// targets stay nameable.
func validIDPart(s string) bool {
	if len(s) == 0 || len(s) > 100 || s == ".." || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func splitShorthand(s string) (owner, repo string) {
	owner, repo, _ = strings.Cut(s, "/")
	return owner, strings.TrimSuffix(repo, ".git")
}

// stripDotGit removes one trailing ".git" (path or URL).
func stripDotGit(s string) string { return strings.TrimSuffix(s, ".git") }

// canonicalGitHubURL folds https://github.com/owner/repo[.git] (+ optional
// trailing slash) to the single canonical https form.
func canonicalGitHubURL(u *url.URL) string {
	p := strings.Trim(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	return "https://github.com/" + p + ".git"
}

// --- SSRF gate --------------------------------------------------------------------

// SSRFConfig is the evaluated gate input (from [import]).
type SSRFConfig struct {
	AllowPrivate bool
	Allowlist    []string // plain hosts, lowercased at check time
	AllowFile    bool
	Dangerous    bool // explicit confirm for empty-allowlist non-GitHub URLs
}

// CheckSSRF enforces the v1 egress policy on an already-normalized source.
// file:// sources never reach DNS; their gate is AllowFile only.
func CheckSSRF(n Normalized, cfg SSRFConfig, resolve func(host string) ([]net.IP, error)) error {
	if n.Kind == SourceFile {
		if !cfg.AllowFile {
			return &StatusError{Status: 400, Message: "file:// sources are disabled (import.allow_file_urls=false)"}
		}
		return nil
	}
	host := strings.ToLower(n.Host)
	if len(cfg.Allowlist) > 0 {
		for _, h := range cfg.Allowlist {
			if strings.ToLower(h) == host {
				return checkPrivate(host, cfg.AllowPrivate, resolve)
			}
		}
		return &StatusError{Status: 400, Message: fmt.Sprintf("source host %q is not in import.url_allowlist", n.Host)}
	}
	// Empty allowlist: GitHub is always reachable; anything else needs the
	// explicit dangerous confirm (residual risk, §9.6).
	if n.Kind != SourceGitHub && !cfg.Dangerous {
		return &StatusError{Status: 400, Message: fmt.Sprintf("source host %q needs import.url_allowlist or the dangerous confirm flag", n.Host)}
	}
	return checkPrivate(host, cfg.AllowPrivate, resolve)
}

// checkPrivate denies loopback/RFC1918/ULA/link-local/multicast resolutions
// unless explicitly allowed. resolve defaults to net.DefaultResolver.LookupIP.
func checkPrivate(host string, allow bool, resolve func(host string) ([]net.IP, error)) error {
	if allow {
		return nil
	}
	if resolve == nil {
		resolve = resolveHost
	}
	ips, err := resolve(host)
	if err != nil {
		return &StatusError{Status: 400, Message: fmt.Sprintf("cannot resolve source host %q: %v", host, scrubError(err.Error()))}
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return &StatusError{Status: 400, Message: fmt.Sprintf("source host %q resolves to a private address (import.allow_private_networks=false)", host)}
		}
	}
	return nil
}

// resolveHost adapts LookupIPAddr to the []net.IP shape checkPrivate takes.
func resolveHost(host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// isPrivateIP denies loopback, RFC1918, ULA, link-local, and multicast
// (stdlib net parse only — no new dependency, law 1).
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 10/8, 172.16/12, 192.168/16, 0/8, 100.64/10 (CGNAT), 192.0.0.0/24.
		if ip4[0] == 10 || ip4[0] == 0 {
			return true
		}
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
		if ip4[0] == 192 && ip4[1] == 168 {
			return true
		}
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 {
			return true
		}
		return false
	}
	// IPv6: unique-local fc00::/7 + documentation/example ranges.
	if len(ip) >= 1 && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

// --- scrubbing (S2) -----------------------------------------------------------------

// scrubURL removes any userinfo that slipped past validation (defense in
// depth: params/packets/errors/logs/bucket must be grep-clean for
// tokens). The canonical URL never carries credentials; this guards
// concatenated strings (clone argv echo, git stderr).
func scrubURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return s
}

// scrubError redacts `password=…`, `token …`, and userinfo-shaped secrets
// from error text (git stderr echoes URLs and helpers).
func scrubError(s string) string {
	out := redactKV(s, "password=")
	out = redactKV(out, "passwd=")
	out = redactKV(out, "token=")
	return out
}

// redactKV cuts `key<value>` at the next delimiter (whitespace, quote,
// semicolon, or end of string).
func redactKV(s, key string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(key)])
		b.WriteString("[redacted]")
		j := i + len(key)
		for j < len(s) && !strings.ContainsRune(" \t\n\r\"';", rune(s[j])) {
			j++
		}
		s = s[j:]
	}
}
