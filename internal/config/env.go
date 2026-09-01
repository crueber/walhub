package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Override is an applied-but-ignored env override, surfaced by `config check`
// (exit 3 under --strict). Overrides that error out (bad value, bad index) are
// fatal instead and travel through Load's error.
type Override struct {
	Name   string // env var name as spelled, e.g. WALGIT__STORE__BKUET
	Key    string // normalized dotted key, e.g. store.bkuet
	Reason string // why it was ignored
}

const (
	envPrefixPrimary = "WALHUB__"
	envPrefixLegacy  = "WALGIT__"
)

var indexPartRe = regexp.MustCompile(`^[0-9]+$`)

type envEntry struct {
	name    string
	raw     string
	primary bool // WALHUB__ spelling (wins over WALGIT__)
}

// applyEnv overlays WALHUB__SECTION__KEY=value (legacy WALGIT__ alias; primary
// wins on conflict) onto c and returns the ignored overrides (11_config_cli.md
// §3.2). getenv backs known special vars; environ is the full environment.
func applyEnv(c *Config, getenv func(string) string, environ []string) ([]Override, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var overrides []Override
	entries := map[string]envEntry{}

	// Legacy alias first so the primary spelling overwrites it.
	for _, pass := range [...]struct {
		prefix  string
		primary bool
	}{
		{envPrefixLegacy, false},
		{envPrefixPrimary, true},
	} {
		for _, kv := range environ {
			name, raw, _ := strings.Cut(kv, "=")
			if !strings.HasPrefix(name, pass.prefix) {
				continue
			}
			rest := name[len(pass.prefix):]
			if rest == "" || strings.Contains(rest, "=") {
				continue
			}
			parts := strings.Split(rest, "__")
			for i := range parts {
				parts[i] = strings.ToLower(parts[i])
			}
			key := strings.Join(parts, ".")
			if prev, ok := entries[key]; ok {
				if prev.primary {
					continue // keep the first primary spelling
				}
				if pass.primary {
					overrides = append(overrides, Override{
						Name:   prev.name,
						Key:    key,
						Reason: "overridden by " + envPrefixPrimary + strings.ToUpper(strings.ReplaceAll(key, ".", "__")),
					})
				}
			}
			entries[key] = envEntry{name: name, raw: raw, primary: pass.primary}
		}
	}

	keys := make([]string, 0, len(entries))
	placementTouched := false
	for key := range entries {
		if strings.HasPrefix(key, "placement.") {
			placementTouched = true
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	applyKeys := func() error {
		for _, key := range keys {
			if !strings.HasPrefix(key, "placement.") {
				continue
			}
			e := entries[key]
			parts := strings.Split(key, ".")
			if err := assignPath(c, parts, e.raw); err != nil {
				if errors.Is(err, errUnknownKey) {
					overrides = append(overrides, Override{Name: e.name, Key: key, Reason: "unknown key " + key})
					continue
				}
				return fmt.Errorf("%s: %w", e.name, err)
			}
		}
		return nil
	}

	// §3.4 placement all-or-nothing: any WALHUB__PLACEMENT__* override takes
	// the whole section from the environment — the file's table is discarded,
	// the four keys are defaulted, then only env-provided keys are assigned.
	if placementTouched {
		c.Placement = Placement{Serve: []string{"*"}, Maintain: []string{"*"}}
		if err := applyKeys(); err != nil {
			return overrides, err
		}
	}

	for _, key := range keys {
		if strings.HasPrefix(key, "placement.") {
			continue
		}
		e := entries[key]
		parts := strings.Split(key, ".")
		if err := assignPath(c, parts, e.raw); err != nil {
			if errors.Is(err, errUnknownKey) {
				overrides = append(overrides, Override{Name: e.name, Key: key, Reason: "unknown key " + key})
				continue
			}
			return overrides, fmt.Errorf("%s: %w", e.name, err)
		}
	}

	// RUST_LOG (kept) overrides telemetry.log_filter; WALHUB_LOG is the
	// new-style spelling. RUST_LOG wins when both are set. These are raw
	// strings, not TOML values.
	if v := getenv("RUST_LOG"); v != "" {
		c.Telemetry.LogFilter = v
	} else if v := getenv("WALHUB_LOG"); v != "" {
		c.Telemetry.LogFilter = v
	}

	return overrides, nil
}

// --- path navigation ---

var errUnknownKey = errors.New("unknown key")

// assignPath sets the field at dotted path parts (segments may be numeric
// array indices) from the raw env value, parsed as a TOML value with a
// raw-string fallback (§3.2).
func assignPath(root *Config, parts []string, raw string) error {
	val, err := parseEnvValue(parts, raw)
	if err != nil {
		return err
	}
	rv := reflect.ValueOf(root).Elem()
	return setAt(rv, parts, val, strings.Join(parts, "."), raw)
}

func setAt(rv reflect.Value, parts []string, val any, path, raw string) error {
	part := parts[0]
	// Numeric segment indexes a slice (array-of-table overrides).
	if indexPartRe.MatchString(part) && rv.Kind() == reflect.Slice {
		idx, _ := strconv.Atoi(part)
		if idx < 0 || idx >= rv.Len() {
			return fmt.Errorf("%s: index %d out of range (%d entries)", path, idx, rv.Len())
		}
		return setAt(rv.Index(idx), parts[1:], val, path, raw)
	}
	if rv.Kind() != reflect.Struct {
		return unknownKeyErr(path)
	}
	fv, ok := fieldByTOMLTag(rv, part)
	if !ok {
		return unknownKeyErr(path)
	}
	if len(parts) == 1 {
		return coerce(fv, val, path, raw)
	}
	return setAt(fv, parts[1:], val, path, raw)
}

func fieldByTOMLTag(rv reflect.Value, name string) (reflect.Value, bool) {
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if tag, _, _ := strings.Cut(f.Tag.Get("toml"), ","); tag == name {
			return rv.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func unknownKeyErr(path string) error {
	return fmt.Errorf("%w: %s", errUnknownKey, path)
}

// --- TOML value parsing (TOML "value" productions only) ---

// parseEnvValue parses raw as a TOML value (strings, integers, floats,
// booleans, arrays, inline tables). A value that is not a valid TOML
// production falls back to a raw string; dates/datetimes are rejected.
func parseEnvValue(parts []string, raw string) (any, error) {
	path := strings.Join(parts, ".")
	var holder struct {
		V any
	}
	_, err := toml.Decode("v = "+raw, &holder)
	if err == nil {
		if _, isTime := holder.V.(time.Time); isTime {
			return nil, fmt.Errorf("%s: dates/datetimes are not accepted", path)
		}
		return holder.V, nil
	}
	if raw == "" {
		return "", nil
	}
	return raw, nil // raw-string fallback
}

// --- value coercion ---

// coerce assigns val (decoded TOML value or raw string) to the field fv,
// applying duration/byte-size parsing and failing closed on type mismatch.
func coerce(fv reflect.Value, val any, path, raw string) error {
	if fv.CanAddr() {
		if u, ok := fv.Addr().Interface().(interface{ UnmarshalText([]byte) error }); ok {
			if err := applyText(u, val); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			return nil
		}
	}
	if err := coerceInto(fv, val, path, raw); err != nil {
		return err
	}
	return nil
}

func applyText(u interface{ UnmarshalText([]byte) error }, val any) error {
	switch v := val.(type) {
	case string:
		return u.UnmarshalText([]byte(v))
	case int64:
		return u.UnmarshalText([]byte(strconv.FormatInt(v, 10)))
	case float64:
		return u.UnmarshalText([]byte(strconv.FormatFloat(v, 'g', -1, 64)))
	case bool:
		return fmt.Errorf("cannot assign boolean %v", v)
	}
	return fmt.Errorf("cannot assign %T", val)
}

func coerceInto(fv reflect.Value, val any, path, raw string) error {
	switch fv.Kind() {
	case reflect.String:
		s, ok := val.(string)
		if !ok {
			return typeErr(path, raw, val, "string")
		}
		fv.SetString(s)
	case reflect.Bool:
		b, ok := val.(bool)
		if !ok {
			return typeErr(path, raw, val, "bool")
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, ok := val.(int64)
		if !ok {
			return typeErr(path, raw, val, "integer")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint64:
		n, ok := val.(int64)
		if !ok || n < 0 {
			return typeErr(path, raw, val, "non-negative integer")
		}
		fv.SetUint(uint64(n))
	case reflect.Float64:
		switch v := val.(type) {
		case float64:
			fv.SetFloat(v)
		case int64:
			fv.SetFloat(float64(v))
		default:
			return typeErr(path, raw, val, "float")
		}
	case reflect.Slice:
		items, ok := val.([]any)
		if !ok {
			return typeErr(path, raw, val, fv.Type().String())
		}
		out := reflect.MakeSlice(fv.Type(), len(items), len(items))
		for i, item := range items {
			if err := coerce(out.Index(i), item, fmt.Sprintf("%s[%d]", path, i), raw); err != nil {
				return err
			}
		}
		fv.Set(out)
	case reflect.Struct:
		fields, ok := val.(map[string]any)
		if !ok {
			return typeErr(path, raw, val, fv.Type().String())
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			f, ok := fieldByTOMLTag(fv, strings.ToLower(name))
			if !ok {
				return unknownKeyErr(path + "." + strings.ToLower(name))
			}
			if err := coerce(f, fields[name], path+"."+strings.ToLower(name), raw); err != nil {
				return err
			}
		}
	default:
		return typeErr(path, raw, val, fv.Type().String())
	}
	return nil
}

func typeErr(path, raw string, val any, want string) error {
	got := raw
	if _, isStr := val.(string); !isStr {
		got = fmt.Sprintf("%v", val)
	}
	return fmt.Errorf("%s: cannot assign %q (%T) to %s", path, got, val, want)
}

// --- PORT lockstep (§3.3) ---

func applyPort(c *Config, getenv func(string) string) error {
	p := getenv("PORT")
	if p == "" {
		return nil
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT: %q is not a TCP port", p)
	}
	host, _, err := net.SplitHostPort(c.Server.Listen) // error = exit 2
	if err != nil {
		return fmt.Errorf("PORT: server.listen %q: %v", c.Server.Listen, err)
	}
	c.Server.Listen = net.JoinHostPort(host, strconv.Itoa(port))
	if u, err := url.Parse(c.Server.PublicURL); err == nil && u.Host != "" && isLoopbackHost(u.Hostname()) {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
		c.Server.PublicURL = u.String()
	}
	return nil
}

// isLoopbackHost: 127.0.0.0/8, ::1, "localhost". A non-loopback public_url
// (the normal proxied case) is NOT touched: PORT is a local-dev /
// reverse-proxy-port knob, not a general URL rewriter.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
