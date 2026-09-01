package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// ConfigFileName / ConfigAliasName are the config file names inside the data
// dir (divergence D5/D8): walhub.toml is written by setup, walgit.toml is
// accepted as an alias (checked second).
const (
	ConfigFileName    = "walhub.toml"
	ConfigAliasName   = "walgit.toml"
	DevNullConfigPath = "/dev/null"
)

// ConfigFilePointer resolves the config-file pointer when --config is absent:
// WALHUB_CONFIG primary, WALGIT_CONFIG legacy fallback (§3.1 step 2, §6.1).
func ConfigFilePointer(getenv func(string) string) (path string, ok bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if p := getenv("WALHUB_CONFIG"); p != "" {
		return p, true
	}
	if p := getenv("WALGIT_CONFIG"); p != "" {
		return p, true
	}
	return "", false
}

// Load runs the loading ladder (11_config_cli.md §3.1) over the default
// location candidates: compiled-in defaults, then the first existing file
// (a missing default file is the zero-config first run, NOT an error), then
// the env overlay, then PORT lockstep, then fail-closed validation.
// filePaths are candidates in priority order — e.g. [<data-dir>/walhub.toml,
// <data-dir>/walgit.toml].
func Load(filePaths []string, env func(string) string) (*Config, []Override, error) {
	return load(filePaths, false, env)
}

// LoadExplicit is the ladder for an explicitly-named config (--config or the
// WALHUB_CONFIG/WALGIT_CONFIG pointer): a missing file is fatal, and
// /dev/null forces defaults+env only.
func LoadExplicit(path string, env func(string) string) (*Config, []Override, error) {
	return load([]string{path}, true, env)
}

func load(filePaths []string, explicit bool, getenv func(string) string) (*Config, []Override, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	dataDir := ResolveDataDir(getenv)
	c := Defaults()

	var overrides []Override
	loaded := false
	if explicit && len(filePaths) == 1 && filePaths[0] == DevNullConfigPath {
		// /dev/null: defaults+env only (§3.1 step 2) — the file leg is skipped.
	} else {
		for _, path := range filePaths {
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if explicit {
						return nil, nil, fmt.Errorf("config file not found: %s", path)
					}
					continue
				}
				return nil, nil, err
			}
			if err := decodeFile(path, c); err != nil {
				return nil, nil, err
			}
			loaded = true
			break
		}
	}
	// Only the default-location ladder (no file found) is a zero-config first
	// run; an explicit /dev/null forces compiled-in defaults+env (§3.1 step 2).
	if !loaded && !explicit {
		c = FirstRunDefaults(dataDir)
	}
	c.DataDir = dataDir

	var err error
	overrides, err = applyEnv(c, getenv, os.Environ())
	if err != nil {
		return c, overrides, err
	}
	if err := applyPort(c, getenv); err != nil {
		return c, overrides, err
	}
	_, verrs := Validate(c)
	if len(verrs) > 0 {
		msgs := make([]string, len(verrs))
		for i, e := range verrs {
			msgs[i] = e.Error()
		}
		return c, overrides, errors.New(strings.Join(msgs, "; "))
	}
	return c, overrides, nil
}

// decodeFile TOML-decodes path into c (seeded from defaults) and fails closed
// on unknown keys, listing key + line (§3.1 step 2).
func decodeFile(path string, c *Config) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	md, err := toml.Decode(string(src), c)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	unknown := md.Undecoded()
	if len(unknown) == 0 {
		return nil
	}
	lines := strings.Split(string(src), "\n")
	msgs := make([]string, 0, len(unknown))
	for _, key := range unknown {
		msgs = append(msgs, fmt.Sprintf("unknown key %q (line %d)", key.String(), findKeyLine(lines, key)))
	}
	return fmt.Errorf("%s: %s", path, strings.Join(msgs, "; "))
}

// findKeyLine finds the 1-based line where a TOML key appears (as `key =` or
// in a table header); 0 when not found.
func findKeyLine(lines []string, key toml.Key) int {
	last := key[len(key)-1]
	full := key.String()
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if eq := strings.Index(t, "="); eq > 0 && !strings.HasPrefix(t, "[") {
			k := strings.Trim(strings.TrimSpace(t[:eq]), `"'`)
			if k == last {
				return i + 1
			}
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			hdr := strings.Trim(t, "[]")
			if hdr == full || headerEndsWith(hdr, last) {
				return i + 1
			}
		}
	}
	return 0
}

func headerEndsWith(hdr, last string) bool {
	idx := strings.LastIndex(hdr, ".")
	tail := hdr
	if idx >= 0 {
		tail = hdr[idx+1:]
	}
	return strings.Trim(tail, `"'`) == last
}

// MarshalTOML serializes the config with the §2 key names (setup save §3.7).
// Duration/ByteSize fields round-trip as strings via MarshalText.
func MarshalTOML(c *Config) ([]byte, error) {
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
