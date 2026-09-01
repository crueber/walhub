package config

import (
	"strings"
	"testing"
	"time"
)

func errsContain(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, err := range errs {
		if strings.Contains(err.Error(), substr) {
			return
		}
	}
	t.Fatalf("errors %v missing %q", errs, substr)
}

func TestValidateDefaultsPass(t *testing.T) {
	warnings, errs := Validate(Defaults())
	if len(errs) != 0 {
		t.Fatalf("compiled-in defaults must validate clean: %v", errs)
	}
	if len(warnings) != 0 {
		t.Fatalf("loopback default must not warn: %v", warnings)
	}
}

// Rule 1 — auth-none loopback is WARN-only (divergence).
func TestValidateAuthNoneNonLoopbackWarns(t *testing.T) {
	c := FirstRunDefaults("/data")
	warnings, errs := Validate(c)
	if len(errs) != 0 {
		t.Fatalf("auth-none on 0.0.0.0 must not fail closed: %v", errs)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "non-loopback listen 0.0.0.0:8080") {
		t.Fatalf("warnings = %v, want loud auth warning", warnings)
	}
	c2 := Defaults()
	c2.Server.Listen = "127.0.0.1:8080"
	c2.Server.Auth.Mode = "none"
	if warnings, _ := Validate(c2); len(warnings) != 0 {
		t.Fatalf("loopback auth-none must not warn: %v", warnings)
	}
}

// Rule 2 — oidc allowlist.
func TestValidateOIDC(t *testing.T) {
	base := func() *Config {
		c := Defaults()
		c.Server.Auth.Mode = "oidc"
		c.Server.Auth.AnonymousRead = false
		c.Server.Auth.AllowedDomains = []string{"example.com"}
		c.Server.Auth.OAuthClientID = "id"
		c.Server.Auth.OAuthClientSecret = "secret"
		return c
	}
	if _, errs := Validate(base()); len(errs) != 0 {
		t.Fatalf("valid oidc config must pass: %v", errs)
	}

	c := base()
	c.Server.Auth.AnonymousRead = true
	_, errs := Validate(c)
	errsContain(t, errs, "anonymous_read must be false")

	c = base()
	c.Server.Auth.AllowedDomains = nil
	c.Server.Auth.AllowedEmails = nil
	_, errs = Validate(c)
	errsContain(t, errs, "allowed_domains or server.auth.allowed_emails")

	c = base()
	c.Server.Auth.OAuthClientSecret = ""
	_, errs = Validate(c)
	errsContain(t, errs, "both set or both unset")

	c = base()
	c.Server.Auth.SessionSecret = "short"
	_, errs = Validate(c)
	errsContain(t, errs, "at least 32 bytes")

	c = base()
	c.Server.Auth.SessionSecret = strings.Repeat("x", 32)
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("32-byte session_secret must pass: %v", errs)
	}
}

// Rule 3 — bundle strategy validation.
func TestValidateBundleStrategies(t *testing.T) {
	if _, errs := Validate(Defaults()); len(errs) != 0 {
		t.Fatalf("default bundle strategies must pass: %v", errs)
	}
	mutate := func(f func(s []BundleStrategy)) []error {
		c := Defaults()
		f(c.Bundles.Strategy)
		_, errs := Validate(c)
		return errs
	}

	errsContain(t, mutate(func(s []BundleStrategy) { s[1].Base = "" }), "incremental strategy requires base")
	errsContain(t, mutate(func(s []BundleStrategy) { s[1].Base = "nonexistent" }), "does not name an earlier-declared strategy")
	errsContain(t, mutate(func(s []BundleStrategy) { s[0].Kind = "incremental"; s[0].Base = "daily" }), "does not name an earlier-declared strategy") // forward ref
	errsContain(t, mutate(func(s []BundleStrategy) { s[1].Keep = 2 }), "keep is a full-strategy concept")
	errsContain(t, mutate(func(s []BundleStrategy) { s[0].Schedule = "not a cron" }), "cron")
	if errs := mutate(func(s []BundleStrategy) { s[0].Schedule = "0 0 23 * * *" }); len(errs) != 0 {
		t.Fatalf("6-field cron must pass: %v", errs)
	}
	errsContain(t, mutate(func(s []BundleStrategy) { s[0].Filter = "blob:something" }), `filter must be "blob:none"`)
	errsContain(t, mutate(func(s []BundleStrategy) { s[0].BackfillMax = -1 }), "backfill_max must be >= 0")
	errsContain(t, mutate(func(s []BundleStrategy) { s[0].MinCommits = -1 }), "min_commits must be >= 0")
	errsContain(t, mutate(func(s []BundleStrategy) { s[0].Refs = []string{"refs/[heads"} }), "does not compile")
	errsContain(t, mutate(func(s []BundleStrategy) { s[1].Name = "weekly" }), "duplicates")
	errsContain(t, mutate(func(s []BundleStrategy) { s[2].Kind = "differential" }), `kind must be "full" or "incremental"`)
}

// Rule 4 — chain-shared filter is all-or-nothing, rejected by name.
func TestValidateBundleChainFilter(t *testing.T) {
	c := Defaults()
	c.Bundles.Strategy[0].Filter = "blob:none" // daily/hourly chain has none
	_, errs := Validate(c)
	errsContain(t, errs, "(daily) mixes filter values in its base chain")

	c = Defaults()
	for i := range c.Bundles.Strategy {
		c.Bundles.Strategy[i].Filter = "blob:none"
	}
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("uniform filtered chain must pass: %v", errs)
	}
}

// Rule 5 — store.
func TestValidateStore(t *testing.T) {
	c := Defaults()
	c.Store.Backend = "ftp"
	_, errs := Validate(c)
	errsContain(t, errs, "store.backend must be one of")

	c = Defaults()
	c.Store.MultipartThreshold = 1 << 20
	c.Store.MultipartPartSize = 2 << 20
	_, errs = Validate(c)
	errsContain(t, errs, "multipart_part_size")

	c = Defaults()
	c.Store.MultipartPartSize = 0 // zero = unset, allowed
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("zero part size must pass: %v", errs)
	}

	c = Defaults()
	c.Store.Backend = "filesystem"
	c.Store.Root = "relative/path"
	_, errs = Validate(c)
	errsContain(t, errs, "store.root must be an absolute path")
}

// Rule 6 — sizes/timeouts: negative values nowhere, watermark in (0,1) or 0.
func TestValidateSizes(t *testing.T) {
	c := Defaults()
	c.Store.MultipartThreshold = -1
	_, errs := Validate(c)
	errsContain(t, errs, "negative value for store.multipart_threshold")

	c = Defaults()
	c.Maintenance.Interval = Duration(-time.Second)
	_, errs = Validate(c)
	errsContain(t, errs, "negative value for maintenance.interval")

	c = Defaults()
	c.Cache.DiskHighWatermark = 1.5
	_, errs = Validate(c)
	errsContain(t, errs, "disk_high_watermark")

	c = Defaults()
	c.Cache.DiskHighWatermark = 0
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("watermark 0 must pass: %v", errs)
	}
}

// Rule 7 — placement globs.
func TestValidatePlacementGlobs(t *testing.T) {
	tests := []struct {
		glob string
		ok   bool
	}{
		{"*", true}, {"owner/*", true}, {"owner/name", true},
		{"", false}, {"owner/*/*", false}, {"**", false}, {"a?b", false}, {"a[b", false}, {"/*", false},
	}
	for _, tt := range tests {
		c := Defaults()
		c.Placement.Serve = []string{tt.glob}
		_, errs := Validate(c)
		if tt.ok && len(errs) != 0 {
			t.Errorf("glob %q: unexpected errors %v", tt.glob, errs)
		}
		if !tt.ok {
			errsContain(t, errs, "placement.serve glob")
		}
	}
}

// Rule 8 — tls.
func TestValidateTLS(t *testing.T) {
	c := Defaults()
	c.Server.TLS.Mode = "files"
	_, errs := Validate(c)
	errsContain(t, errs, "requires server.tls.cert and server.tls.key")

	c = Defaults()
	c.Server.TLS.Mode = "bogus"
	_, errs = Validate(c)
	errsContain(t, errs, "server.tls.mode must be one of")

	c = Defaults()
	c.Server.TLS.Mode = "self_signed"
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("self_signed implies nothing else: %v", errs)
	}
}

// Rule 9 — roles.
func TestValidateRoles(t *testing.T) {
	c := Defaults()
	c.Server.Roles = []string{"serve", "maintain", "events", "serve"} // duplicates allowed
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("valid roles must pass: %v", errs)
	}
	c.Server.Roles = []string{"admin"}
	_, errs := Validate(c)
	errsContain(t, errs, "must be one of serve|maintain|events")
}

// Rule 10 — paths.
func TestValidatePaths(t *testing.T) {
	c := Defaults()
	c.Cache.Dir = "relative/dir"
	_, errs := Validate(c)
	errsContain(t, errs, "cache.dir must be an absolute path")

	c.Store.Backend = "memory"
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("memory backend exempts cache.dir: %v", errs)
	}
}

func TestValidCron(t *testing.T) {
	valid := []string{
		"@hourly", "@daily", "@weekly", "@monthly", "@yearly",
		"0 0 23 * * Sun", "0 0 * * * *", "*/5 * * * * *", "0 0 0 1 Jan *", "0 0 23 * * Sun",
		"0,15,30,45 * * * * *", "0 0 0-12 * * *",
	}
	for _, s := range valid {
		if !validCron(s) {
			t.Errorf("validCron(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "0 0 23 * *", "0 0 23 * * * *", "not a cron", "0 0 23 * * Sundaylong", "* * * * *"}
	for _, s := range invalid {
		if validCron(s) {
			t.Errorf("validCron(%q) = true, want false", s)
		}
	}
}

func TestValidRefGlobCases(t *testing.T) {
	for g, want := range map[string]bool{
		"refs/heads/*": true, "main": true, "[abc]def": true, "": false,
		"refs/[heads": false, "heads]": false, `refs\heads`: false,
	} {
		if got := validRefGlob(g); got != want {
			t.Errorf("validRefGlob(%q) = %v, want %v", g, got, want)
		}
	}
}

func TestResolveDataDirContainer(t *testing.T) {
	t.Setenv("WALHUB_DATA_DIR", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if got := ResolveDataDir(func(string) string { return "" }); got != "/var/lib/walhub" {
		t.Fatalf("container data dir = %q, want /var/lib/walhub", got)
	}
}

func TestValidateMalformedListen(t *testing.T) {
	c := Defaults()
	c.Server.Listen = "no-port-here"
	warnings, errs := Validate(c)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(warnings) != 1 {
		t.Fatalf("unparseable listen must not count as loopback: %v", warnings)
	}
}
