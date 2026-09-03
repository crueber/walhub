package config

import (
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strings"
)

// Validate runs every §5 rule: exit-2-fatal errors plus at most one warning.
// auth `none` on a non-loopback bind returns a warning, not an error. Everything
// else is exit-2-fatal at boot.
func Validate(c *Config) (warnings []string, errs []error) {
	if w := checkAuthNoneLoopback(c); w != "" {
		warnings = append(warnings, w)
	}
	errs = append(errs, checkOIDC(c)...)
	errs = append(errs, checkBundleStrategies(c)...)
	errs = append(errs, checkBundleChainFilters(c)...)
	errs = append(errs, checkStore(c)...)
	errs = append(errs, checkSizes(c)...)
	errs = append(errs, checkPlacementGlobs(c)...)
	errs = append(errs, checkTLS(c)...)
	errs = append(errs, checkRoles(c)...)
	errs = append(errs, checkPaths(c)...)
	errs = append(errs, checkSSH(c)...)
	return warnings, errs
}

// checkSSH (17_ssh.md §3): the transport is disabled unless listen is set;
// user keys are not config (they live in the object store, managed through
// the UI/API), so only the listener shape is validated here.
func checkSSH(c *Config) []error {
	sc := c.Server.SSH
	if sc.Listen == "" && sc.HostKey == "" && sc.HostKeyEnv == "" {
		return nil
	}
	var errs []error
	if sc.Listen != "" {
		if _, _, err := net.SplitHostPort(sc.Listen); err != nil {
			errs = append(errs, fmt.Errorf("server.ssh.listen %q must be host:port", sc.Listen))
		}
	}
	return errs
}

// 1. none-mode loopback (DIVERGENCE — warn, not fail).
func checkAuthNoneLoopback(c *Config) string {
	if c.Server.Auth.Mode != "none" {
		return ""
	}
	if isLoopbackListen(c.Server.Listen) {
		return ""
	}
	return fmt.Sprintf(
		"auth.mode=none on non-loopback listen %s; anyone who can reach this port can read and write every repository — set server.auth.mode = \"token\" (or oidc) and restart",
		c.Server.Listen)
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

// 2. oidc allowlist.
func checkOIDC(c *Config) []error {
	a := c.Server.Auth
	if a.Mode != "oidc" {
		return nil
	}
	var errs []error
	if a.AnonymousRead {
		errs = append(errs, fmt.Errorf("server.auth.anonymous_read must be false when auth.mode = \"oidc\""))
	}
	if len(a.AllowedDomains) == 0 && len(a.AllowedEmails) == 0 {
		errs = append(errs, fmt.Errorf("auth.mode = \"oidc\" requires server.auth.allowed_domains or server.auth.allowed_emails"))
	}
	if (a.OAuthClientID == "") != (a.OAuthClientSecret == "") {
		errs = append(errs, fmt.Errorf("server.auth.oauth_client_id and server.auth.oauth_client_secret must be both set or both unset"))
	}
	if a.SessionSecret != "" && len(a.SessionSecret) < 32 {
		errs = append(errs, fmt.Errorf("server.auth.session_secret must be at least 32 bytes (got %d)", len(a.SessionSecret)))
	}
	return errs
}

// 3. bundle strategy validation.
func checkBundleStrategies(c *Config) []error {
	var errs []error
	for i, s := range c.Bundles.Strategy {
		at := fmt.Sprintf("bundles.strategy[%d]", i)
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name is required", at))
		} else {
			for j := range i {
				if c.Bundles.Strategy[j].Name == s.Name {
					errs = append(errs, fmt.Errorf("%s.name %q duplicates bundles.strategy[%d]", at, s.Name, j))
				}
			}
		}
		switch s.Kind {
		case "full":
		case "incremental":
			if s.Base == "" {
				errs = append(errs, fmt.Errorf("%s: incremental strategy requires base", at))
			} else {
				found := false
				for j := range i {
					if c.Bundles.Strategy[j].Name == s.Base {
						found = true
						break
					}
				}
				if !found {
					errs = append(errs, fmt.Errorf("%s: base %q does not name an earlier-declared strategy", at, s.Base))
				}
			}
			if s.Keep != 0 {
				errs = append(errs, fmt.Errorf("%s: keep is a full-strategy concept and fails on an incremental strategy", at))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.kind must be \"full\" or \"incremental\" (got %q)", at, s.Kind))
		}
		if !validCron(s.Schedule) {
			errs = append(errs, fmt.Errorf("%s.schedule %q is not a valid 6-field cron expression or @macro", at, s.Schedule))
		}
		if s.Filter != "" && s.Filter != "blob:none" {
			errs = append(errs, fmt.Errorf("%s.filter must be \"blob:none\" (got %q)", at, s.Filter))
		}
		if s.BackfillMax < 0 {
			errs = append(errs, fmt.Errorf("%s.backfill_max must be >= 0", at))
		}
		if s.MinCommits < 0 {
			errs = append(errs, fmt.Errorf("%s.min_commits must be >= 0", at))
		}
		for _, ref := range s.Refs {
			if !validRefGlob(ref) {
				errs = append(errs, fmt.Errorf("%s.refs glob %q does not compile", at, ref))
			}
		}
	}
	return errs
}

// 4. chain-shared filter: a strategy and its transitive base chain MUST
// declare identical filter values (absent = none); mixing is rejected by name.
func checkBundleChainFilters(c *Config) []error {
	var errs []error
	strategies := c.Bundles.Strategy
	for i, s := range strategies {
		filters := map[string]bool{s.Filter: true}
		name := s.Base
		for range len(strategies) {
			if name == "" {
				break
			}
			var base *BundleStrategy
			for j := range i {
				if strategies[j].Name == name {
					base = &strategies[j]
					break
				}
			}
			if base == nil {
				break
			}
			filters[base.Filter] = true
			name = base.Base
		}
		if len(filters) > 1 {
			errs = append(errs, fmt.Errorf("bundles.strategy[%d] (%s) mixes filter values in its base chain: a filtered chain is all-or-nothing", i, s.Name))
		}
	}
	return errs
}

// 5. store.
func checkStore(c *Config) []error {
	var errs []error
	switch c.Store.Backend {
	case "s3", "gcs", "memory", "filesystem":
	default:
		errs = append(errs, fmt.Errorf("store.backend must be one of s3|gcs|memory|filesystem (got %q)", c.Store.Backend))
	}
	if c.Store.MultipartPartSize > 0 && c.Store.MultipartThreshold > 0 &&
		c.Store.MultipartPartSize > c.Store.MultipartThreshold {
		errs = append(errs, fmt.Errorf("store.multipart_part_size (%s) must be <= store.multipart_threshold (%s)",
			c.Store.MultipartPartSize, c.Store.MultipartThreshold))
	}
	if c.Store.Backend == "filesystem" && c.Store.Root != "" && !strings.HasPrefix(c.Store.Root, "/") {
		errs = append(errs, fmt.Errorf("store.root must be an absolute path or empty (got %q)", c.Store.Root))
	}
	return errs
}

// 6. sizes/timeouts: negative values nowhere (durations/sizes already parsed
// by the typed decoders), watermark in (0,1) or 0.
func checkSizes(c *Config) []error {
	var errs []error
	var walk func(rv reflect.Value, path string)
	walk = func(rv reflect.Value, path string) {
		rt := rv.Type()
		for i := range rt.NumField() {
			f := rt.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if tag == "-" {
				continue
			}
			at := tag
			if path != "" {
				at = path + "." + tag
			}
			fv := rv.Field(i)
			switch fv.Kind() {
			case reflect.Struct:
				walk(fv, at)
			case reflect.Int64:
				switch fv.Type() {
				case reflect.TypeOf(Duration(0)):
					if Duration(fv.Int()) < 0 {
						errs = append(errs, fmt.Errorf("negative value for %s", at))
					}
				case reflect.TypeOf(ByteSize(0)):
					if ByteSize(fv.Int()) < 0 {
						errs = append(errs, fmt.Errorf("negative value for %s", at))
					}
				}
			}
		}
	}
	walk(reflect.ValueOf(c).Elem(), "")
	if w := c.Cache.DiskHighWatermark; w != 0 && (w <= 0 || w >= 1) {
		errs = append(errs, fmt.Errorf("cache.disk_high_watermark must be in (0,1) or 0 (got %v)", w))
	}
	return errs
}

// 7. placement globs: `*`, `owner/*`, or `owner/name` (one / at most).
func checkPlacementGlobs(c *Config) []error {
	var errs []error
	lists := [...]struct {
		key  string
		vals []string
	}{
		{"placement.serve", c.Placement.Serve},
		{"placement.serve_exclude", c.Placement.ServeExclude},
		{"placement.maintain", c.Placement.Maintain},
		{"placement.maintain_exclude", c.Placement.MaintainExclude},
	}
	for _, l := range lists {
		for _, g := range l.vals {
			if !validPlacementGlob(g) {
				errs = append(errs, fmt.Errorf("%s glob %q must be *, owner/*, or owner/name", l.key, g))
			}
		}
	}
	return errs
}

func validPlacementGlob(g string) bool {
	if g == "" || strings.ContainsAny(g, "?[]\\") {
		return false
	}
	owner, pattern, hasSlash := strings.Cut(g, "/")
	if hasSlash {
		if strings.Contains(pattern, "/") {
			return false // one / at most
		}
		if owner == "" {
			return false
		}
		return pattern == "*" || !strings.ContainsAny(pattern, "*")
	}
	return g == "*"
}

// 8. tls.
func checkTLS(c *Config) []error {
	var errs []error
	switch c.Server.TLS.Mode {
	case "off", "self_signed":
	case "files":
		if c.Server.TLS.Cert == "" || c.Server.TLS.Key == "" {
			errs = append(errs, fmt.Errorf("server.tls.mode = \"files\" requires server.tls.cert and server.tls.key"))
		}
	default:
		errs = append(errs, fmt.Errorf("server.tls.mode must be one of off|self_signed|files (got %q)", c.Server.TLS.Mode))
	}
	return errs
}

// 9. roles.
func checkRoles(c *Config) []error {
	var errs []error
	for _, r := range c.Server.Roles {
		switch r {
		case "serve", "maintain", "events":
		default:
			errs = append(errs, fmt.Errorf("server.roles entry %q must be one of serve|maintain|events", r))
		}
	}
	return errs
}

// 10. paths: cache.dir absolute (Rust requires this for the tls/ sibling)
// unless the store backend is memory.
func checkPaths(c *Config) []error {
	if c.Store.Backend == "memory" {
		return nil
	}
	if !strings.HasPrefix(c.Cache.Dir, "/") {
		return []error{fmt.Errorf("cache.dir must be an absolute path (got %q)", c.Cache.Dir)}
	}
	return nil
}

// --- schedule / glob syntax (parse-only; the real cron parser lives in
// 08_bundles.md / internal/bundle) ---

var cronFieldRe = regexp.MustCompile(`^\*(/[0-9]+)?$|^([0-9]+|[A-Za-z]{3})(-([0-9]+|[A-Za-z]{3}))?(/[0-9]+)?$`)

// validCron reports whether schedule parses as a 6-field UTC cron expression
// or one of the @macros.
func validCron(s string) bool {
	switch s {
	case "@hourly", "@daily", "@weekly", "@monthly", "@yearly":
		return true
	}
	fields := strings.Fields(s)
	if len(fields) != 6 {
		return false
	}
	for _, f := range fields {
		for _, alt := range strings.Split(f, ",") {
			if !cronFieldRe.MatchString(alt) {
				return false
			}
		}
	}
	return true
}

// validRefGlob checks a git ref glob (owner-free) compiles: non-empty,
// balanced brackets, no stray escapes.
func validRefGlob(g string) bool {
	if g == "" {
		return false
	}
	depth := 0
	for _, r := range g {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return false
			}
		case '\\':
			return false
		}
	}
	return depth == 0
}
