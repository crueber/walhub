// url_test.go — normalization, transport, SSRF, scrubbing, and the S4
// refmap (all table-driven, no network, no git).
package repoimport

import (
	"net"
	"strings"
	"testing"
)

func TestNormalizeSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		url  string
		kind SourceKind
		host string
		fail bool
	}{
		{name: "shorthand", in: "acme/monorepo", url: "https://github.com/acme/monorepo.git", kind: SourceGitHub, host: "github.com"},
		{name: "shorthand dotgit", in: "acme/monorepo.git", url: "https://github.com/acme/monorepo.git", kind: SourceGitHub, host: "github.com"},
		{name: "github https", in: "https://github.com/acme/monorepo", url: "https://github.com/acme/monorepo.git", kind: SourceGitHub, host: "github.com"},
		{name: "github https dotgit", in: "https://github.com/acme/monorepo.git", url: "https://github.com/acme/monorepo.git", kind: SourceGitHub, host: "github.com"},
		{name: "github trailing slash", in: "https://github.com/acme/monorepo/", url: "https://github.com/acme/monorepo.git", kind: SourceGitHub, host: "github.com"},
		{name: "generic https", in: "https://git.example.com/team/proj.git", url: "https://git.example.com/team/proj", kind: SourceGeneric, host: "git.example.com"},
		{name: "file", in: "file:///srv/git/r.git", url: "file:///srv/git/r.git", kind: SourceFile},
		{name: "ssh scheme", in: "ssh://example.com/team/proj.git", kind: SourceGeneric, host: "example.com"},
		{name: "ssh userinfo refused", in: "ssh://git@example.com/team/proj.git", fail: true},
		{name: "scp-like", in: "git@github.com:acme/monorepo.git", kind: SourceGeneric},
		{name: "git proto", in: "git://example.com/proj.git", kind: SourceGeneric, host: "example.com"},
		{name: "empty", in: "", fail: true},
		{name: "spaces", in: "   ", fail: true},
		{name: "no scheme", in: "git.example.com/team/proj", fail: true},
		{name: "bad scheme", in: "ftp://example.com/proj", fail: true},
		{name: "embedded creds", in: "https://user:pass@github.com/acme/r.git", fail: true},
		{name: "embedded token", in: "https://x-access-token:tok@github.com/acme/r.git", fail: true},
		{name: "shorthand bad charset", in: "ac me/repo", fail: true},
		{name: "shorthand leading dot", in: ".acme/repo", fail: true},
		{name: "shorthand deep", in: "a/b/c", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := NormalizeSource(tc.in)
			if tc.fail {
				if err == nil {
					t.Fatalf("NormalizeSource(%q) = %+v, want error", tc.in, n)
				}
				if _, ok := err.(*StatusError); !ok {
					t.Fatalf("error type = %T, want *StatusError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSource(%q) error: %v", tc.in, err)
			}
			if n.URL != tc.url && tc.url != "" {
				t.Fatalf("URL = %q, want %q", n.URL, tc.url)
			}
			if n.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", n.Kind, tc.kind)
			}
			if tc.host != "" && n.Host != tc.host {
				t.Fatalf("Host = %q, want %q", n.Host, tc.host)
			}
		})
	}
}

func TestValidateTransport(t *testing.T) {
	mk := func(scheme string) Normalized {
		return Normalized{URL: scheme + "://h/r.git", Scheme: scheme, Host: "h"}
	}
	for _, tc := range []struct {
		name     string
		n        Normalized
		token    bool
		failPart string
	}{
		{name: "https", n: mk("https")},
		{name: "https token", n: mk("https"), token: true},
		{name: "http", n: mk("http")},
		{name: "http token refused", n: mk("http"), token: true, failPart: "never send credentials"},
		{name: "ssh refused", n: mk("ssh"), failPart: "not supported"},
		{name: "scp refused", n: Normalized{URL: "git@h:r.git", Scheme: "scp"}, failPart: "not supported"},
		{name: "git refused", n: mk("git"), failPart: "not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransport(tc.n, tc.token)
			if tc.failPart == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.failPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failPart) {
					t.Fatalf("err = %v, want containing %q", err, tc.failPart)
				}
				if se, ok := err.(*StatusError); !ok || se.Status != 400 {
					t.Fatalf("status = %v, want 400", err)
				}
			}
		})
	}
}

func TestCheckSSRF(t *testing.T) {
	pub := func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	priv := func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil }
	loop := func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }
	v6ula := func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("fd00::1")}, nil }
	dnserr := func(host string) ([]net.IP, error) { return nil, errTestDNS }
	gh := Normalized{URL: "https://github.com/a/r.git", Kind: SourceGitHub, Host: "github.com", Scheme: "https"}
	gen := Normalized{URL: "https://git.example.com/r.git", Kind: SourceGeneric, Host: "git.example.com", Scheme: "https"}
	file := Normalized{URL: "file:///x", Kind: SourceFile, Scheme: "file"}
	for _, tc := range []struct {
		name     string
		n        Normalized
		cfg      SSRFConfig
		resolve  func(string) ([]net.IP, error)
		failPart string
	}{
		{name: "github default", n: gh, resolve: pub},
		{name: "github private blocked", n: gh, resolve: priv, failPart: "private address"},
		{name: "github private allowed", n: gh, cfg: SSRFConfig{AllowPrivate: true}, resolve: priv},
		{name: "generic needs allowlist-or-dangerous", n: gen, resolve: pub, failPart: "dangerous confirm"},
		{name: "generic dangerous", n: gen, cfg: SSRFConfig{Dangerous: true}, resolve: pub},
		{name: "generic allowlisted", n: gen, cfg: SSRFConfig{Allowlist: []string{"git.example.com"}}, resolve: pub},
		{name: "generic allowlisted loopback blocked", n: gen, cfg: SSRFConfig{Allowlist: []string{"git.example.com"}}, resolve: loop, failPart: "private address"},
		{name: "generic not allowlisted", n: gen, cfg: SSRFConfig{Allowlist: []string{"other.example"}}, resolve: pub, failPart: "not in import.url_allowlist"},
		{name: "v6 ula blocked", n: gen, cfg: SSRFConfig{Dangerous: true}, resolve: v6ula, failPart: "private address"},
		{name: "dns failure", n: gen, cfg: SSRFConfig{Dangerous: true}, resolve: dnserr, failPart: "cannot resolve"},
		{name: "file allowed", n: file, cfg: SSRFConfig{AllowFile: true}},
		{name: "file denied", n: file, failPart: "allow_file_urls=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSSRF(tc.n, tc.cfg, tc.resolve)
			if tc.failPart == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.failPart != "" && (err == nil || !strings.Contains(err.Error(), tc.failPart)) {
				t.Fatalf("err = %v, want containing %q", err, tc.failPart)
			}
		})
	}
}

type testDNSErr struct{}

func (testDNSErr) Error() string { return "no such host" }

var errTestDNS = testDNSErr{}

func TestScrub(t *testing.T) {
	if got := scrubURL("https://user:tok@github.com/a/r.git"); strings.Contains(got, "tok") {
		t.Fatalf("scrubURL leaked userinfo: %q", got)
	}
	for _, tc := range []struct{ in, want string }{
		{`clone: password=s3cr3t failed`, `clone: password=[redacted] failed`},
		{`token=abc123 Forged`, `token=[redacted] Forged`},
		{`nothing secret here`, `nothing secret here`},
		{`password=x password=y`, `password=[redacted] password=[redacted]`},
		{`tail password=zzz`, `tail password=[redacted]`},
	} {
		if got := scrubError(tc.in); got != tc.want {
			t.Fatalf("scrubError(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsPullHead(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"refs/pull/42/head", true},
		{"refs/pull/1/head", true},
		{"refs/pull/42/merge", false},
		{"refs/pull/head", false},
		{"refs/pull//head", false},
		{"refs/pull/4x/head", false},
		{"refs/pull/42/head/extra", false},
		{"refs/heads/main", false},
	} {
		if got := isPullHead(tc.in); got != tc.want {
			t.Fatalf("isPullHead(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFilterRefs(t *testing.T) {
	all := []Ref{
		{Name: "refs/heads/main", Oid: "a1"},
		{Name: "refs/heads/topic", Oid: "a2"},
		{Name: "refs/tags/v1", Oid: "a3", Peeled: "a4"},
		{Name: "refs/pull/7/head", Oid: "a5"},
		{Name: "refs/pull/7/merge", Oid: "a6"},
		{Name: "refs/changes/1", Oid: "a7"},
		{Name: "refs/review/x", Oid: "a8"},
		{Name: "refs/notes/commits", Oid: "a9"},
		{Name: "refs/replace/deadbeef", Oid: "aa"},
		{Name: "refs/meta/config", Oid: "ab"},
		{Name: "refs/keep-around/x", Oid: "ac"},
	}
	names := func(refs []Ref) []string {
		out := []string{}
		for _, r := range refs {
			out = append(out, r.Name)
		}
		return out
	}
	join := func(refs []Ref) string { return strings.Join(names(refs), ",") }
	if got := join(FilterRefs(all, false, false, nil, false, "")); got != "refs/heads/main,refs/heads/topic,refs/tags/v1" {
		t.Fatalf("default refmap = %q", got)
	}
	if got := join(FilterRefs(all, true, true, nil, false, "")); got != "refs/heads/main,refs/heads/topic,refs/tags/v1,refs/pull/7/head,refs/notes/commits" {
		t.Fatalf("opt-in refmap = %q", got)
	}
	if got := join(FilterRefs(all, false, false, []string{"refs/tags/v1"}, false, "")); got != "refs/tags/v1" {
		t.Fatalf("requested refmap = %q", got)
	}
	if got := join(FilterRefs(all, false, false, nil, true, "refs/heads/topic")); got != "refs/heads/topic" {
		t.Fatalf("default-branch-only refmap = %q", got)
	}
	if got := FilterRefs(nil, false, false, nil, false, ""); len(got) != 0 {
		t.Fatalf("nil in must stay non-nil empty, got %v", got)
	}
	// Peel survives the filter.
	for _, r := range FilterRefs(all, false, false, nil, false, "") {
		if r.Name == "refs/tags/v1" && r.Peeled != "a4" {
			t.Fatalf("peel lost: %+v", r)
		}
	}
}

func TestCredentialArgv(t *testing.T) {
	if got := credentialArgv("https", "github.com", ""); got != nil {
		t.Fatalf("no token must mean no helper, got %v", got)
	}
	argv := credentialArgv("https", "github.com", "WALGIT_IMPORT_TOKEN_x")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "credential.https://github.com.helper=") {
		t.Fatalf("helper not host-pinned: %q", joined)
	}
	if strings.Contains(joined, "s3cr3t") {
		t.Fatalf("token must never appear in argv: %q", joined)
	}
	if !strings.Contains(joined, "$WALGIT_IMPORT_TOKEN_x") {
		t.Fatalf("helper must reference the env name: %q", joined)
	}
	env := CredentialEnv("i0123456789abcdef-xyz!")
	if strings.ContainsAny(env, "-!") {
		t.Fatalf("env name must be shell-safe: %q", env)
	}
	if !strings.HasPrefix(env, "WALGIT_IMPORT_TOKEN_") {
		t.Fatalf("env prefix wrong: %q", env)
	}
}
